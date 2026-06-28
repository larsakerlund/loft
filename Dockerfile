# Backend image: loftd alone, the API daemon. A reverse proxy in front (see web/ for a local one,
# or your own gateway) handles ingress and auth. loftd is a single static Go binary, so this stays
# tiny and cold-starts fast.
#
# loftd listens on all interfaces here so the proxy container can reach it over the network. It is
# not trusted-open: loftd validates the forwarded access token itself (audience + scope + azp), so
# the API stays closed even to an internal caller.
#
# Build context is the repo root: docker build -t loftd .

FROM golang:1.26.4-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /loftd ./cmd/loftd

FROM alpine:3.22
# Pull the latest alpine security patches on top of the base image, so a CVE fixed in the package
# repo but not yet in a base rebuild does not ship. Scanned in CI (and locally) before release.
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates
COPY --from=build /loftd /usr/local/bin/loftd

ENV LOFT_LISTEN=0.0.0.0:8082
EXPOSE 8082
ENTRYPOINT ["/usr/local/bin/loftd"]
