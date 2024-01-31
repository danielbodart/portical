FROM docker:latest

RUN apk update &&  \
    apk upgrade &&  \
    apk add miniupnpc bash

COPY run /opt/portical/

ENTRYPOINT ["/bin/sh", "-c"]
CMD ["/opt/portical/run", "poll"]