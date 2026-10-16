#!/bin/sh
set -eu

if [ "$#" -eq 0 ]; then
    set -- serve
fi

if [ "$#" -eq 1 ] && [ "$1" = "serve" ] && [ ! -e config.yaml ]; then
    umask 077
    cp /etc/ws-file-share/config.example.yaml config.yaml
fi

exec /usr/local/bin/ws-downloader "$@"
