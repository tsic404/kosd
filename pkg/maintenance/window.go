package maintenance

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
)

// WindowSettingID is the cluster-scoped policy item SETTINGS-C-72 defines: the
// weekly period in which a maintenance start is allowed. The product declares
// the item (C-23); the flow reads it by the identity the contract fixes.
const WindowSettingID = "cluster-maintenance-window"

// weekMinutes is one week in minutes, the unit a window reads a week in.
const weekMinutes = 7 * 24 * 60

// WindowPolicy reads the declared cluster policy the flow judges (C-23, C-72).
// declared is false when no active component contributes the item, which leaves
// maintenance unrestricted the way an unwritten value does.
type WindowPolicy interface {
	ClusterValue(ctx context.Context, id string) (value string, declared bool, failure *controlplane.Error)
}

// Window is the weekly period a maintenance start is allowed in (C-72): the
// days and times the policy's entries name, judged in UTC. An empty window
// allows every instant, which is the unwritten policy.
type Window struct {
	spans []windowSpan
}

// windowSpan is one allowed interval in minutes since Sunday 00:00. An interval
// that ends before it starts continues past midnight; an interval that runs past
// the week's end is read again through the following week.
type windowSpan struct {
	start int
	end   int
}

// weekdays maps the three-letter day names to the minutes a week starts them
// at; a window value uses these names so the stored value stays readable.
var weekdays = map[string]int{
	"sun": 0 * 24 * 60,
	"mon": 1 * 24 * 60,
	"tue": 2 * 24 * 60,
	"wed": 3 * 24 * 60,
	"thu": 4 * 24 * 60,
	"fri": 5 * 24 * 60,
	"sat": 6 * 24 * 60,
}

// ParseWindow reads the weekly period a user wrote through the control-plane
// API (C-72). The value is one or more "Sun 02:00-04:00" periods separated by
// commas; times are 24-hour clock and judge UTC. An empty value is an unwritten
// policy and allows every instant.
func ParseWindow(value string) (Window, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return Window{}, nil
	}
	segments := strings.Split(trimmed, ",")
	window := Window{spans: make([]windowSpan, 0, len(segments))}
	for _, segment := range segments {
		span, err := parseSpan(strings.TrimSpace(segment))
		if err != nil {
			return Window{}, err
		}
		window.spans = append(window.spans, span)
	}
	return window, nil
}

// parseSpan reads one "Sun 02:00-04:00" period.
func parseSpan(segment string) (windowSpan, error) {
	fields := strings.Fields(segment)
	if len(fields) != 2 {
		return windowSpan{}, invalidSpan(segment)
	}
	day, ok := weekdays[strings.ToLower(fields[0])]
	if !ok {
		return windowSpan{}, invalidSpan(segment)
	}
	bounds := strings.Split(fields[1], "-")
	if len(bounds) != 2 {
		return windowSpan{}, invalidSpan(segment)
	}
	start, err := clockMinutes(bounds[0])
	if err != nil {
		return windowSpan{}, invalidSpan(segment)
	}
	end, err := clockMinutes(bounds[1])
	if err != nil {
		return windowSpan{}, invalidSpan(segment)
	}
	if start == end {
		return windowSpan{}, invalidSpan(segment)
	}
	finish := day + end
	if finish < day+start {
		finish += 24 * 60
	}
	return windowSpan{start: day + start, end: finish}, nil
}

// clockMinutes reads one 24-hour clock time as minutes after midnight. The
// shape is fixed to HH:MM so one period cannot be stored in two spellings.
func clockMinutes(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, fmt.Errorf("clock time %q is not HH:MM", value)
	}
	for index, character := range value {
		if index == 2 {
			continue
		}
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("clock time %q is not HH:MM", value)
		}
	}
	at, err := time.Parse("15:04", value)
	if err != nil {
		return 0, err
	}
	return at.Hour()*60 + at.Minute(), nil
}

// invalidSpan is the reason a stored window cannot be judged. It names the
// accepted shape because the policy is the user's write.
func invalidSpan(segment string) error {
	return fmt.Errorf("时段 %q 无效，应写作 Sun 02:00-04:00；多个时段用逗号分隔", segment)
}

// Allows reports whether one instant falls inside the window. An empty window
// allows every instant; an end boundary is outside, so two adjacent periods do
// not overlap.
func (w Window) Allows(at time.Time) bool {
	if len(w.spans) == 0 {
		return true
	}
	minute := minuteOfWeek(at)
	for _, span := range w.spans {
		if span.contains(minute) {
			return true
		}
	}
	return false
}

// contains reports whether one minute since Sunday 00:00 falls inside the span,
// reading the span in the current week and through the following week so an
// interval ending past the week's end still covers its tail.
func (s windowSpan) contains(minute int) bool {
	return (minute >= s.start && minute < s.end) ||
		(minute+weekMinutes >= s.start && minute+weekMinutes < s.end)
}

// NextOpening returns the first instant the window opens after at. A caller
// asks only when Allows reported at outside the window, so the answer is always
// in the future.
func (w Window) NextOpening(at time.Time) time.Time {
	base := weekStart(at)
	var next time.Time
	for _, span := range w.spans {
		for _, start := range []int{span.start, span.start + weekMinutes} {
			candidate := base.Add(time.Duration(start) * time.Minute)
			if !candidate.After(at) {
				continue
			}
			if next.IsZero() || candidate.Before(next) {
				next = candidate
			}
		}
	}
	return next
}

// minuteOfWeek places one instant on the week a time.Weekday indexes from
// Sunday, judged in UTC so the stored value means the same thing everywhere.
func minuteOfWeek(at time.Time) int {
	utc := at.UTC()
	return int(utc.Weekday())*24*60 + utc.Hour()*60 + utc.Minute()
}

// weekStart is Sunday 00:00 UTC of the week at falls in.
func weekStart(at time.Time) time.Time {
	utc := at.UTC()
	day := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -int(utc.Weekday()))
}

// judgeWindow refuses a maintenance start the cluster policy puts outside its
// window (C-72). An unwritten policy and a policy no active component declares
// both leave the start unrestricted. A refusal is returned before the flow
// writes anything, so the node's unschedulable field and the audit trail keep
// their state.
func (s *Service) judgeWindow(ctx context.Context, node string) *controlplane.Error {
	if s.Window == nil {
		return nil
	}
	value, declared, failure := s.Window.ClusterValue(ctx, WindowSettingID)
	if failure != nil {
		return failure
	}
	if !declared || strings.TrimSpace(value) == "" {
		return nil
	}
	window, err := ParseWindow(value)
	if err != nil {
		return &controlplane.Error{
			Code:               controlplane.CodeConflict,
			Message:            "维护窗口策略无法解析，维护不能发起：" + err.Error(),
			Stage:              controlplane.StageValidate,
			Object:             &controlplane.ObjectRef{Group: api.Group, Kind: "Node", Name: node},
			RequiresUserAction: true,
			SideEffects:        controlplane.SideEffectsNone,
		}
	}
	now := s.now()
	if window.Allows(now) {
		return nil
	}
	return windowClosed(window.NextOpening(now), now, node)
}

// windowClosed is the refusal of a start outside the policy window (C-72): the
// reason names the next opening, and the retry delay states how long the caller
// must wait for it.
func windowClosed(next, now time.Time, node string) *controlplane.Error {
	wait := next.Sub(now)
	seconds := int64(wait / time.Second)
	if wait%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return &controlplane.Error{
		Code:              controlplane.CodeConflict,
		Message:           "本次维护发起在维护窗口外，下次开窗时点为 " + next.UTC().Format(time.RFC3339),
		Stage:             controlplane.StageValidate,
		Object:            &controlplane.ObjectRef{Group: api.Group, Kind: "Node", Name: node},
		Retryable:         true,
		RetryAfterSeconds: seconds,
		SideEffects:       controlplane.SideEffectsNone,
	}
}
