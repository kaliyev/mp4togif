# build
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/mp4togif ./...

# runtime
FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates
WORKDIR /app
COPY --from=build /out/mp4togif /app/mp4togif
EXPOSE 8080
ENV ADDR=:8080
ENV VIDEOS_DIR=/tmp/videos/
CMD ["/app/mp4togif"]