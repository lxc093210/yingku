FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/yingku ./cmd/yingku

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata ffmpeg
COPY --from=build /out/yingku /usr/local/bin/yingku
ENV YINGKU_ADDR=:8097 YINGKU_DATA=/data/yingku.db
VOLUME /data
EXPOSE 8097
ENTRYPOINT ["yingku"]
