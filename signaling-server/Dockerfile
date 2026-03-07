FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o signaling-server .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /app/signaling-server /usr/local/bin/signaling-server
RUN mkdir -p /data
EXPOSE 8080
CMD ["signaling-server"]
