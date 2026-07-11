# syntax=docker/dockerfile:1

# ---- build stage ----------------------------------------------------------
FROM golang:1.25-alpine AS build

# git is only needed if modules are fetched from VCS; kept for completeness.
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache modules first for faster incremental builds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
# CGO disabled -> fully static binary; strip symbols for size.
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/agent-beacon ./cmd/agent-beacon

# ---- runtime stage --------------------------------------------------------
FROM alpine:3.20 AS runtime

# git is required at runtime for the spawn/worktree feature.
RUN apk add --no-cache ca-certificates git tzdata \
    && addgroup -S beacon \
    && adduser -S -G beacon -h /home/beacon beacon

COPY --from=build /out/agent-beacon /usr/local/bin/agent-beacon

USER beacon
WORKDIR /home/beacon

EXPOSE 8080
ENTRYPOINT ["agent-beacon"]
CMD ["server", "--address", ":8080"]
