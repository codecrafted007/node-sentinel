# Security Policy

## Reporting a vulnerability

Please report security issues **privately** — do not open a public issue for a vulnerability.

Use GitHub's [private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability) ("Report a vulnerability" under the repository's **Security** tab), or contact the maintainer directly.

Please include:
- a description of the issue and its impact,
- steps to reproduce (or a proof of concept),
- affected version / commit.

We'll acknowledge within a few days and keep you updated on the fix.

## Scope & threat model notes

node-sentinel is a **privileged, kernel-facing** component — worth keeping in mind:

- The agent loads eBPF programs and runs with elevated capabilities (`CAP_BPF`, `CAP_PERFMON`, `CAP_SYS_RESOURCE`, `CAP_SYS_PTRACE`) and `hostPID`. It reads the host's CRI socket and cgroup tree.
- It is **observe-only today** — it does not taint, cordon, or evict. Remediation (which *would* act on workloads) is on the roadmap and will land behind explicit confidence gates and policy.
- A core safety rule: a cgroup that can't be tied to a real CRI container resolves to `unknown` and is **never attributed** — the system prefers "can't tell" over blaming the wrong pod.

Reports about privilege escalation, unsafe attribution leading to wrongful action, or container-boundary issues are especially welcome.

## Supported versions

The project is pre-1.0; security fixes target the `main` branch.
