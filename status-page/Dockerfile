FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/status-page .

# Distroless: no shell, no package manager, runs as non-root
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/status-page /status-page
COPY config.example.json /config.json
EXPOSE 8080
ENTRYPOINT ["/status-page", "-config", "/config.json"]
