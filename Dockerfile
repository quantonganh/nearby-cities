FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY nearby-cities .
EXPOSE 8080
ENTRYPOINT [ "./nearby-cities" ]