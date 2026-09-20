FROM golang:1.26-alpine AS builder

WORKDIR /app

# Install gcc and musl-dev for CGO (if required for SQLite)
RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o crypdog ./cmd/server/main.go

FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /app/crypdog .
# Production configuration and secrets must be supplied at runtime. Do not bake
# credentials or a local development config into the image.

EXPOSE 8080

CMD ["./crypdog"]
