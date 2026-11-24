# Image for the worst-case chaos scenarios (docs/TESTING.md, "Worst cases").
# Go + the tools the disk and network faults need. The tests run as the
# non-root user "tester" (PostgreSQL refuses root); tester may run only
# mount, tc and strace as root, inside this container.
FROM golang:1.27.4-trixie
RUN apt-get update && apt-get install -y --no-install-recommends strace sudo iproute2 procps psmisc util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -m -u 1000 tester \
 && printf 'tester ALL=(root) NOPASSWD: /usr/bin/mount, /usr/bin/umount, /usr/sbin/tc, /usr/bin/strace, /usr/bin/kill\n' > /etc/sudoers.d/tester \
 && chmod 0440 /etc/sudoers.d/tester
USER tester
ENV HOME=/home/tester GOFLAGS=-buildvcs=false
