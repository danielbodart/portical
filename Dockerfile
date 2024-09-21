FROM --platform=${BUILDPLATFORM} docker:latest

RUN apk add --no-cache --update bash miniupnpc

COPY run /opt/portical/

ENTRYPOINT []
CMD ["/opt/portical/run", "listen"]
