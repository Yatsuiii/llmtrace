FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o llmtrace ./cmd/llmtrace

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/llmtrace .
COPY entrypoint.sh .
RUN chmod +x entrypoint.sh
RUN mkdir -p /data && chmod 777 /data
EXPOSE 8080
ENTRYPOINT ["./entrypoint.sh"]
