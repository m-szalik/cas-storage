# syntax=docker/dockerfile:1
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VER=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cas-storage ./cmd/cas-storage

FROM gcr.io/distroless/static:nonroot
ARG VER=dev
LABEL version=$VER
COPY --from=build /out/cas-storage /app
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
