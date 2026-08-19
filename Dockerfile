# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

# Cross-compile to whatever the target platform is rather than assuming amd64.
ARG TARGETOS
ARG TARGETARCH
# No BuildKit cache mounts: Railway requires a literal id=s/<service-id>-<path>
# on every cache mount, which would hardcode one service into this repo.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/autoscaler ./cmd/autoscaler

# distroless static ships CA certificates (needed for the Railway API over TLS)
# and a nonroot user, without a shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/autoscaler /autoscaler

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/autoscaler"]
