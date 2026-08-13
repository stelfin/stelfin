# Single static binary, small container, no node_modules — the ops posture
# DESIGN.md commits to in section 1. Nothing in the runtime image can be
# "logged into" or shelled into for debugging on purpose: a payments server
# should be trivial to audit and hard to tamper with from inside its own
# container.

FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first so they cache across builds that only touch source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/stelfind ./cmd/stelfind

# distroless "static" carries CA certificates (Horizon, the Anthropic API and
# the Meta Graph API are all called over HTTPS) and a built-in non-root user,
# nothing else — no shell, no package manager, no attack surface beyond the
# binary itself.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/stelfind /stelfind

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/stelfind"]
