FROM alpine:latest
RUN apk add --no-cache gcompat
RUN addgroup -S clusterbulb && adduser -S clusterbulb -G clusterbulb
COPY go-clusterbulb /go-clusterbulb
ENTRYPOINT ["/go-clusterbulb"]
