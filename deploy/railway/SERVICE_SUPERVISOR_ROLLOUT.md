# Service supervisor rollout

The image change replaces PID-only service liveness with verified Linux process identity. It also replaces the model-facing filesystem, git, and exec MCP servers with Neo's in-process native filesystem, bounded shell, durable named-service, and read-only git tools. Substantial asynchronous project coding remains a durable private AgentCore Build job; the legacy exec file remains admin-only for registry audit, recovery, and cleanup. Remaining remote integration adapters degrade independently if unavailable instead of terminating the daemon.

No production mutation is automatic. Build and publish the daemon image through the normal repository workflow, then roll the four audited services one at a time:

```bash
./deploy/railway/rollout-service-supervisor.sh order

export MATRIX_RAILWAY_ROLLOUT_APPROVED=YES
./deploy/railway/rollout-service-supervisor.sh redeploy matrix-d1015d90-c85f-4e84-8ca6-08
./deploy/railway/rollout-service-supervisor.sh verify matrix-d1015d90-c85f-4e84-8ca6-08
```

Continue in the printed order only after health, required AgentCore configuration and binary checks, the native local-tool runtime inventory gate, the read-only Build-job API probe, both legacy registries, the bounded recovery directories, production-manifest absence of `fs`, `git`, and `exec`, and the non-mutating recovery-route probe pass verification. The Build API probe only lists durable jobs; it does not create or run one. The recovery probe deliberately sends an invalid confirmation value and requires HTTP 400, so `verify` cannot clear data or stop work. The first deployment uses the default `report` policy: unsafe commands are redacted and reported but are not blocked. Rotate exposed credentials and replace inline values with approved secret references before enabling enforcement:

```bash
export MATRIX_RAILWAY_ROLLOUT_APPROVED=YES
./deploy/railway/rollout-service-supervisor.sh enforce <service>
./deploy/railway/rollout-service-supervisor.sh verify <service>
```

`enforce` refuses to proceed while either registry still contains credential material. It then sets `MATRIX_EXEC_INLINE_SECRET_POLICY=block` and requests a redeploy. Do not promote past a service with PID aliases, legacy unverified PIDs, stale active-shell records, or an autostart entry that is not identity-verified and running.

Use the read-only fleet audit at any time:

```bash
./deploy/railway/audit-service-supervisors.sh
```
