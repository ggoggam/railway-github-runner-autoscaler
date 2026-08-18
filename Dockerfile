# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY *.go ./

# Cross-compile to whatever the target platform is rather than assuming amd64.
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/autoscaler .

# distroless static ships CA certificates (needed for the Railway API over TLS)
# and a nonroot user, without a shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/autoscaler /autoscaler

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/autoscaler"]
