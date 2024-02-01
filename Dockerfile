FROM docker:latest

RUN apk update &&  \
    apk upgrade &&  \
    apk add miniupnpc bash

COPY run /opt/portical/

ENTRYPOINT []
CMD ["/opt/portical/run", "poll"]