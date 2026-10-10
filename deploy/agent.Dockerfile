# Build with Docker Buildx for linux/amd64 and linux/arm64, then pin the
# published image by digest in the operator-owned profile.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/spawnpod-agent ./cmd/spawnpod-agent
COPY internal/supervisor ./internal/supervisor
COPY internal/engine ./internal/engine
COPY internal/workspace ./internal/workspace
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /spawnpod-agent ./cmd/spawnpod-agent

FROM scratch
COPY --from=build /spawnpod-agent /spawnpod-agent
USER 10001:10001
ENTRYPOINT ["/spawnpod-agent"]
