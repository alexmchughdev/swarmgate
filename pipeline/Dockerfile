# Pinned by digest rather than the floating :stable tag so this fixture's
# content is reproducible; re-pin by running:
#   docker pull busybox:stable && docker inspect --format='{{index .RepoDigests 0}}' busybox:stable
FROM busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662
ARG CONTENT_FILE=hello.txt
COPY ${CONTENT_FILE} /hello.txt
CMD ["sleep", "3600"]
