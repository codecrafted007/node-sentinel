---
name: Bug report
about: Report a problem with node-sentinel
title: ""
labels: bug
assignees: ""
---

## What happened

<!-- A clear description of the bug and what you expected instead. -->

## Reproduce

<!-- Steps, command line, or manifest used. -->

## Environment

- node-sentinel version / commit:
- Kernel (`uname -r`):
- BTF present (`ls /sys/kernel/btf/vmlinux`):
- cgroups (`stat -fc %T /sys/fs/cgroup`):
- Container runtime (containerd / CRI-O + version):
- Kubernetes (if applicable):
- Arch (amd64 / arm64):

## Logs

<!-- Agent/controller logs. For BPF load failures, include the verifier/BTF error. -->

```
paste here
```
