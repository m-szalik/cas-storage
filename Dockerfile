# syntax=docker/dockerfile:1
# The build runs on the builder's platform and cross-compiles, so multi-arch
# images need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VER=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/cas-storage ./cmd/cas-storage

FROM gcr.io/distroless/static:nonroot
ARG VER=dev
LABEL version=$VER
COPY --from=build /out/cas-storage /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
