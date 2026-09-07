# pgmuxd as a container image.
#
# The point of an image over the static binary is the digest: `make deploy`
# rsyncing a file leaves nothing but a startup log line to say which build is
# live, and a digest-pinned image says it in the job file.

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Same flags as scripts/build.sh. CGO off so the binary needs no libc, and
# -trimpath so the build path does not end up in it.
RUN CGO_ENABLED=0 GOOS=linux GOARCH="$TARGETARCH" go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.buildDate=$BUILD_DATE" \
      -o /pgmuxd ./cmd/pgmuxd

# alpine, not scratch. The binary is static and would run happily on scratch,
# but /healthz is loopback-only by design, so nothing outside the network
# namespace can probe it — the health check has to run *inside* the container,
# and that needs a shell and an HTTP client. A scratch image passes every test
# and then silently cannot be health-checked.
#
# Pinned by digest because this is what ships. busybox wget is already here and
# is the right one: GNU wget retries on 5xx by default, so a health check
# against a draining or unhealthy pgmuxd would hang until its timeout instead of
# failing fast.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

# Links the package to the repository, which is what makes it inherit the
# repository's visibility instead of arriving unlinked and public-by-default.
LABEL org.opencontainers.image.source="https://github.com/boringSQL/pgmux"
LABEL org.opencontainers.image.description="PostgreSQL routing proxy"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=build /pgmuxd /usr/local/bin/pgmuxd

# Unprivileged and numeric: nothing here needs a passwd entry, and 5432 is above
# 1024 so no capability is required to bind it.
USER 65534:65534

ENTRYPOINT ["/usr/local/bin/pgmuxd"]
