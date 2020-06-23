FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
COPY crc-cluster-bot /usr/bin/
ENTRYPOINT ["/usr/bin/crc-cluster-bot"]
