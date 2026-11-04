# Build from the repository root: docker build -f examples/demo/Dockerfile .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /demo ./examples/demo

FROM alpine:3.22
COPY --from=build /demo /demo
EXPOSE 8080 6060
ENTRYPOINT ["/demo"]
