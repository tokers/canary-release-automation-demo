FROM golang:1.19 AS build-env
LABEL maintainer="zchao1995@gmail.com"

ARG ENABLE_PROXY=false

RUN rm -rf /etc/localtime \
    && ln -s /usr/share/zoneinfo/Hongkong /etc/localtime \
    && dpkg-reconfigure -f noninteractive tzdata

WORKDIR /build
COPY go.* ./

RUN if [ "$ENABLE_PROXY" = "true" ] ; then go env -w GOPROXY=https://goproxy.cn,direct ; fi \
    && go mod download

COPY . .
RUN go build -o error-page

FROM centos:centos7
LABEL maintainer="zchao1995@gmail.com"

WORKDIR /error-page
RUN yum -y install ca-certificates libc6-compat \
    && update-ca-trust \
    && echo "hosts: files dns" > /etc/nsswitch.conf

COPY --from=build-env /build/error-page .
COPY --from=build-env /usr/share/zoneinfo/Hongkong /etc/localtime

ENTRYPOINT ["/error-page/error-page"]
