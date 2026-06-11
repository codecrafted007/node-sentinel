# node-sentinel container image.
#
# The binaries are built ahead of time with ./build.sh on a Linux host that has
# kernel BTF — they embed the compiled BPF bytecode (go:embed) and are static
# (CGO_ENABLED=0). This image just packages them; there is no toolchain inside.
# One image carries all three binaries: the agent is the default entrypoint, the
# controller Deployment overrides the command, and sentinelctl is run via
# `kubectl exec` into the agent pod.
#
#   ./build.sh                                  # -> bin/{agent,controller,sentinelctl}
#   docker build -t node-sentinel:dev .
#
# CO-RE means the BPF bytecode relocates against the running kernel at load time,
# so an image built on one kernel (>= 5.10 with BTF) runs on others.
FROM gcr.io/distroless/static-debian12

COPY bin/agent       /usr/local/bin/agent
COPY bin/controller  /usr/local/bin/controller
COPY bin/sentinelctl /usr/local/bin/sentinelctl

ENTRYPOINT ["/usr/local/bin/agent"]
