# syntax=docker/dockerfile:1

# Both base images are pinned by digest; Dependabot keeps them current.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/zitadel-netbird-sync .

# No shell, no package manager, runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/zitadel-netbird-sync /zitadel-netbird-sync
USER 65532:65532
ENTRYPOINT ["/zitadel-netbird-sync"]
