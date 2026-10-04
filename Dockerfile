# syntax=docker/dockerfile:1

# ---- build ----
# Pinned by digest so a re-tag upstream cannot change what we build with.
FROM golang:1.27@sha256:e0174e51e81218523251d85d248a90d24c3d5e81543b4f07a5d66229397db190 AS build
WORKDIR /src

# Dependencies first so this layer caches across source changes.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY cmd ./cmd
COPY internal ./internal
# Static, reproducible-ish binary: no cgo, no local paths, no symbol table.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -buildid=" -o /out/floorsvc ./cmd/floorsvc

# ---- runtime ----
# distroless/static: no shell, no package manager, CA certs + tzdata only.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/floorsvc /floorsvc
# Numeric UID so Kubernetes can verify runAsNonRoot without resolving names.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/floorsvc"]
