# syntax=docker/dockerfile:1

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/linkpulse ./cmd/linkpulse

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/linkpulse /usr/local/bin/linkpulse
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/linkpulse"]
