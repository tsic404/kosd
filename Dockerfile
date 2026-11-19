# The control-plane artifact. The published tag must be the reference
# pkg/install renders (install.DefaultImage), so what an installation pulls is
# exactly what this file builds.
FROM golang:1.27-alpine AS build

WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/kos-control-plane ./cmd/kos-control-plane

FROM alpine:3.22

# The process keeps its database beside itself; the directory exists in the
# image so a deployment may mount a volume over it or run without one.
RUN adduser -D -u 65532 -h /var/lib/kosd kos \
    && mkdir -p /var/lib/kosd \
    && chown -R 65532:65532 /var/lib/kosd
COPY --from=build /out/kos-control-plane /usr/local/bin/kos-control-plane

USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/kos-control-plane"]
