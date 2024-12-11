FROM golang:1.23-alpine AS builder
WORKDIR /app

# Copy just go.mod and go.sum first
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the rest of your files
COPY . .

# Build the binary
RUN go build -o volumescaler .

FROM alpine:3.17
WORKDIR /app
COPY --from=builder /app/volumescaler .
ENTRYPOINT ["./volumescaler"]