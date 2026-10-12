#!/usr/bin/env bash
set -u
id
command -v docker dockerd sudo
ls -l /dev/kvm /var/run/docker.sock 2>/dev/null || true
cat /proc/self/cgroup
cat /sys/fs/cgroup/cgroup.controllers || true
findmnt /sys/fs/cgroup || true
docker version || true
docker info || true
sudo -n true && echo sudo-available || true
if test -e /dev/kvm; then echo kvm-present; else echo kvm-absent; fi
if docker info >/dev/null 2>&1; then
    docker run --rm --privileged --cgroupns=host alpine:3.21 sh -ec '
      mount -t tmpfs tmpfs /mnt
      echo privileged-mount-ok
      mkdir /sys/fs/cgroup/bbval-probe
      rmdir /sys/fs/cgroup/bbval-probe
      echo privileged-cgroup-write-ok
    '
fi
