FROM alpine:latest
RUN apk add --no-cache gcompat
COPY go-clusterbulb /go-clusterbulb
ENTRYPOINT ["/go-clusterbulb"]
