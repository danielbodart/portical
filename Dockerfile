FROM docker:latest

RUN apk update &&  \
    apk upgrade &&  \
    apk add miniupnpc bash getopt

COPY run /opt/portical/

CMD ["/opt/portical/run", "poll"]