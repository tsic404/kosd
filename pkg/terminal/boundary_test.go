package terminal

import (
	"context"
	"testing"

	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/install"
)

// TestBoundaryJudgesProductionIsolation covers ADR-043 and KOSD-C-93 on the
// production judge: a Pod whose container can read the control plane's
// credential or an application sensitive parameter is refused, while a Pod
// carrying neither is admitted.
func TestBoundaryJudgesProductionIsolation(t *testing.T) {
	container := func(fields map[string]any) []any {
		entry := map[string]any{"name": "app", "image": "busybox:1.36"}
		for key, value := range fields {
			entry[key] = value
		}
		return []any{entry}
	}
	cases := []struct {
		name     string
		boundary Boundary
		observed controlplane.TerminalBoundaryTarget
		refused  bool
	}{
		{
			name:     "plain Pod",
			observed: boundaryPod(map[string]any{"containers": container(nil)}, nil, false),
		},
		{
			name: "service account token projection",
			observed: boundaryPod(map[string]any{
				"volumes": []any{map[string]any{
					"name": "token",
					"projected": map[string]any{"sources": []any{
						map[string]any{"serviceAccountToken": map[string]any{"path": "token"}},
					}},
				}},
				"containers": container(map[string]any{
					"volumeMounts": []any{map[string]any{
						"name": "token", "mountPath": "/var/run/secrets/kubernetes.io/serviceaccount",
					}},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name: "config map projection",
			observed: boundaryPod(map[string]any{
				"volumes": []any{map[string]any{
					"name": "configuration",
					"projected": map[string]any{"sources": []any{
						map[string]any{"configMap": map[string]any{"name": "app"}},
					}},
				}},
				"containers": container(map[string]any{
					"volumeMounts": []any{map[string]any{
						"name": "configuration", "mountPath": "/configuration",
					}},
				}),
			}, nil, false),
		},
		{
			name: "control-plane service account",
			observed: boundaryPod(map[string]any{
				"serviceAccountName": install.ControlPlaneServiceAccount,
				"containers":         container(nil),
			}, nil, false),
			refused: true,
		},
		{
			name: "control-plane workload label",
			observed: boundaryPod(map[string]any{"containers": container(nil)}, map[string]any{
				"app": install.ControlPlaneName,
			}, false),
			refused: true,
		},
		{
			name: "managed credential volume",
			observed: boundaryPod(map[string]any{
				"volumes": []any{map[string]any{
					"name":   "managed-credential",
					"secret": map[string]any{"secretName": install.ManagedCredentialSecret},
				}},
				"containers": container(map[string]any{
					"volumeMounts": []any{map[string]any{
						"name": "managed-credential", "mountPath": "/etc/kos/managed",
					}},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name: "managed credential environment",
			observed: boundaryPod(map[string]any{
				"containers": container(map[string]any{
					"env": []any{map[string]any{
						"name": "KUBECONFIG", "value": "/etc/kos/managed/kubeconfig",
					}},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name:     "sensitive application configuration",
			observed: boundaryPod(map[string]any{"containers": container(nil)}, nil, true),
			refused:  true,
		},
		{
			name: "secret environment reference",
			observed: boundaryPod(map[string]any{
				"containers": container(map[string]any{
					"env": []any{map[string]any{
						"name": "PASSWORD",
						"valueFrom": map[string]any{
							"secretKeyRef": map[string]any{"name": "app", "key": "password"},
						},
					}},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name: "secret volume mount",
			observed: boundaryPod(map[string]any{
				"volumes": []any{map[string]any{
					"name":   "credentials",
					"secret": map[string]any{"secretName": "app"},
				}},
				"containers": container(map[string]any{
					"volumeMounts": []any{map[string]any{"name": "credentials", "mountPath": "/credentials"}},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name: "config map mount",
			observed: boundaryPod(map[string]any{
				"volumes": []any{map[string]any{
					"name":      "configuration",
					"configMap": map[string]any{"name": "app"},
				}},
				"containers": container(map[string]any{
					"volumeMounts": []any{map[string]any{"name": "configuration", "mountPath": "/configuration"}},
				}),
			}, nil, false),
		},
		{
			name: "host path mount",
			observed: boundaryPod(map[string]any{
				"volumes": []any{map[string]any{
					"name":     "host",
					"hostPath": map[string]any{"path": "/etc/kos"},
				}},
				"containers": container(map[string]any{
					"volumeMounts": []any{map[string]any{"name": "host", "mountPath": "/host"}},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name: "privileged container",
			observed: boundaryPod(map[string]any{
				"containers": container(map[string]any{
					"securityContext": map[string]any{"privileged": true},
				}),
			}, nil, false),
			refused: true,
		},
		{
			name: "unreadable Pod",
			observed: controlplane.TerminalBoundaryTarget{Target: controlplane.TerminalTarget{
				Type: controlplane.TerminalTargetTypePod, ID: "pod",
				Namespace: "apps", Container: "app",
			}},
			refused: true,
		},
		{
			name: "node without declared isolation",
			observed: controlplane.TerminalBoundaryTarget{Target: controlplane.TerminalTarget{
				Type: controlplane.TerminalTargetTypeNode, ID: "node-a",
			}},
			refused: true,
		},
		{
			name:     "node with declared isolation",
			boundary: Boundary{NodeIsolated: true},
			observed: controlplane.TerminalBoundaryTarget{Target: controlplane.TerminalTarget{
				Type: controlplane.TerminalTargetTypeNode, ID: "node-a",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failure := tc.boundary.Judge(context.Background(), tc.observed)
			if !tc.refused {
				if failure != nil {
					t.Fatalf("judge refused an isolated target: %+v", failure)
				}
				return
			}
			if failure == nil {
				t.Fatal("judge admitted a target whose isolation cannot be guaranteed")
			}
			if failure.Code != controlplane.CodeTerminalCredentialBoundaryUnavailable {
				t.Fatalf("refusal code = %s, want %s", failure.Code, controlplane.CodeTerminalCredentialBoundaryUnavailable)
			}
			if failure.Stage != controlplane.StageConnect || failure.SideEffects != controlplane.SideEffectsNone {
				t.Fatalf("refusal envelope = stage %s side effects %s", failure.Stage, failure.SideEffects)
			}
		})
	}
}

// boundaryPod builds one Pod fixture for the production judge.
func boundaryPod(spec map[string]any, labels map[string]any, sensitive bool) controlplane.TerminalBoundaryTarget {
	metadata := map[string]any{"name": "pod", "namespace": "apps"}
	if labels != nil {
		metadata["labels"] = labels
	}
	pod := map[string]any{"metadata": metadata, "spec": spec}
	return controlplane.TerminalBoundaryTarget{
		Target: controlplane.TerminalTarget{
			Type: controlplane.TerminalTargetTypePod, ID: "pod",
			Namespace: "apps", Container: "app",
		},
		Pod:       pod,
		Sensitive: sensitive,
	}
}
