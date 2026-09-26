# Container image for the BDT relayer (watcher + withdraw worker + confirmation
# poller + sweeper + solvency monitor). One binary, five goroutines.
#
# Used by heroku.yml, and works anywhere else that runs containers.
#
#   docker build -t bdt-relayer .
#   docker run --env-file relayer/.env bdt-relayer
#
# The go.mod lives in relayer/, not at the repo root, which is why this file is
# here rather than relying on a Heroku Go buildpack — the buildpack would not
# find the module.

# ---- build ----
FROM golang:1.25-alpine AS build

# git is needed by `go mod download` for some module paths.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy the manifests first so the (slow) dependency download is cached and only
# re-runs when go.mod/go.sum actually change.
COPY relayer/go.mod relayer/go.sum ./
RUN go mod download

COPY relayer/ ./

# CGO_ENABLED=0 gives a fully static binary that runs on any base image.
# go-ethereum's default build uses the pure-Go KZG implementation, so no C
# toolchain is required as long as the `ckzg` build tag is NOT set.
#
# -trimpath keeps absolute build paths out of the binary; -s -w drops the symbol
# table, which is smaller and gives an attacker less to work with.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relayer ./cmd \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/console ./cmd/api

# ---- runtime ----
FROM alpine:3.20

# ca-certificates is required to speak HTTPS to the RPC endpoint and to Postgres
# over TLS. tzdata so timestamps in logs are not stuck in UTC-only builds.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 relayer

COPY --from=build /out/relayer  /usr/local/bin/relayer
COPY --from=build /out/console  /usr/local/bin/console

# Never run the process that holds the hot wallet key as root.
USER relayer

# No EXPOSE and no port: the relayer is a worker, it listens for nothing. If you
# also run the console (`console`), it reads $PORT.
ENTRYPOINT ["/usr/local/bin/relayer"]
