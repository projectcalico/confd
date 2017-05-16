FROM alpine:latest
MAINTAINER Tom Denham <tom@projectcalico.org>

# netcat is used a simple docker REST client to trigger container restarts and send signals to bird
RUN apk -U add netcat-openbsd

# Hostname also needs to be correct - see start.sh
ENV ETCD_AUTHORITY localhost:2379

ADD conf.d /conf.d
ADD templates /templates
ADD start.sh /usr/local/bin
ADD bin/confd confd 

# Ensure that bird has enough config to start
RUN mkdir /config
RUN echo "protocol device {}" > /config/bird.cfg
RUN echo "protocol device {}" > /config/bird6.cfg
VOLUME /config

CMD ["start.sh"]
