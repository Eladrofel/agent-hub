# Multi-stage build: compile the Go binary, then run it on a minimal base.
FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/agent-hub ./cmd/agent-hub

FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /out/agent-hub /usr/local/bin/agent-hub
EXPOSE 8787 8788

ENTRYPOINT ["/usr/local/bin/agent-hub"]
CMD ["serve"]
