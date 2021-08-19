FROM alpine:3.11.2

RUN apk add --no-cache ca-certificates
ADD bin/kubevirt-cloud-controller-manager /bin/

ENV KUBEVIRT_CLIENT_GO_SCHEME_REGISTRATION_VERSION=v1

ENTRYPOINT ["/bin/kubevirt-cloud-controller-manager"]
