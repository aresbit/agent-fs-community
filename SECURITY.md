# Security

Agent-FS Community is a trusted-user, local-development tool. It has no HTTP
authentication, multi-tenant authorization, file ACL policy, or audit trail.
The CLI rejects daemon listeners that are not loopback addresses.

Do not expose port 7337 through a public reverse proxy, SSH reverse tunnel,
container port mapping, shared workstation, or untrusted local process.

Report vulnerabilities privately to the repository maintainers. Include the
Agent-FS version, operating system, reproduction steps, and whether an
untrusted local process or remote connection is required.
