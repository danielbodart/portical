FROM docker:latest

RUN apk update &&  \
    apk upgrade &&  \
    apk add miniupnpc

COPY run /run
RUN chmod u+x ./run

