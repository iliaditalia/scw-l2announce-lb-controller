# Build on the native platform and let Go cross-compile via TARGETOS/TARGETARCH.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

RUN apk update && apk add --no-cache ca-certificates && update-ca-certificates

WORKDIR /go/src/github.com/iliaditalia/scw-l2announce-lb-controller

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY cmd/ cmd/
COPY l2lb/ l2lb/

ARG TAG
ARG COMMIT_SHA
ARG BUILD_DATE
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-w -s -X github.com/iliaditalia/scw-l2announce-lb-controller/l2lb.version=${TAG} -X github.com/iliaditalia/scw-l2announce-lb-controller/l2lb.buildDate=${BUILD_DATE} -X github.com/iliaditalia/scw-l2announce-lb-controller/l2lb.gitCommit=${COMMIT_SHA} " -o scw-l2announce-lb-controller ./cmd/scw-l2announce-lb-controller

FROM scratch
WORKDIR /
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /go/src/github.com/iliaditalia/scw-l2announce-lb-controller/scw-l2announce-lb-controller .
ENTRYPOINT ["/scw-l2announce-lb-controller"]
