# Control plane image: webhook receiver, scheduler, hostlink server, and the
# operator API with metering and billing. Build from the repository root:
#   docker build -f deploy/controlplane/Dockerfile -t tachyon-controlplane .
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/tachyon-controlplane ./cmd/tachyon-controlplane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tachyon-controlplane /usr/local/bin/tachyon-controlplane
# 8080 webhooks, 9090 hostlink (gRPC, mTLS), 8081 operator API (keep private).
EXPOSE 8080 9090 8081
ENTRYPOINT ["/usr/local/bin/tachyon-controlplane"]
