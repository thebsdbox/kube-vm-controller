# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/manager ./cmd && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/fake-daemon ./cmd/fake-daemon

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/manager /manager
# Only used by config/e2e; harmless otherwise.
COPY --from=build /out/fake-daemon /fake-daemon
USER 65532:65532
ENTRYPOINT ["/manager"]
