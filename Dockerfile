# Build stage
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -o /out/opencode-go-proxy .

# Runtime stage
FROM gcr.io/distroless/base-debian12
COPY --from=build /out/opencode-go-proxy /opencode-go-proxy
EXPOSE 8082
ENTRYPOINT ["/opencode-go-proxy", "-config", "/etc/opencode-go-proxy/config.json"]
