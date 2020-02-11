#!/bin/sh

IMAGE_NAME=svallero/htcontroller:latest
# build and push image
make docker-build docker-push IMG=${IMAGE_NAME}
# deploy
make deploy IMG=${IMAGE_NAME}
