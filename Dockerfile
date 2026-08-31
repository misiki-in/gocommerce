# One binary in one image: the API, the admin panel and every migration.
#
#   docker build -t gocommerce .
#   docker run -p 8080:8080 -e DATABASE_URL=... gocommerce
#
# The panel is rebuilt from source here rather than taken from the committed
# `admin/build`. The committed copy exists so `go build` works on a machine with
# no Node; an image built from a checkout has Node available for the length of
# one stage, and building it means the image can never ship a panel older than
# the source beside it.

# ------------------------------------------------------------------- panel
FROM node:22-alpine AS panel
WORKDIR /src/admin

# The manifest alone first, so a change to a Svelte file does not re-resolve
# the dependency tree.
COPY admin/package.json admin/package-lock.json ./
RUN npm ci

COPY admin/ ./
RUN npm run build

# ------------------------------------------------------------------ binary
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# The panel built above replaces the committed one, which the COPY of the repo
# has just put here.
COPY --from=panel /src/admin/build ./admin/build

# Static, so the final image needs no libc, and -trimpath keeps build paths out
# of it. The version is deliberately not stamped: gocommerce.Version is a const,
# so the linker's -X would be silently ignored and the image would claim a
# version it does not have.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w" -o /out/gocommerce ./cmd/gocommerce

# Somewhere for uploads, created here so that its ownership is right. Docker
# seeds a fresh named volume from the image's directory, ownership included, so
# preparing it in the build is what lets the container run as a non-root user
# and still write to a mounted volume.
RUN mkdir -p /out/media

# ------------------------------------------------------------------- image
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/gocommerce /gocommerce
COPY --from=build --chown=nonroot:nonroot /out/media /data/media

ENV GOCOMMERCE_MEDIA_DIR=/data/media
EXPOSE 8080
USER nonroot

# No HEALTHCHECK: distroless has no shell, and `gocommerce doctor` checks a
# store's configuration rather than whether this process is answering. The
# liveness endpoint is GET /health, which is what a proxy should watch.
ENTRYPOINT ["/gocommerce"]
CMD ["-addr", "0.0.0.0:8080", "serve"]
