# Service supervisor rollout

The image change replaces PID-only service liveness with verified Linux process identity. It also adds redacted registry audits and a staged policy for rejecting credential-bearing commands.

No production mutation is automatic. Build and publish the daemon image through the normal repository workflow, then roll the four audited services one at a time:

```bash
./deploy/railway/rollout-service-supervisor.sh order

export MATRIX_RAILWAY_ROLLOUT_APPROVED=YES
./deploy/railway/rollout-service-supervisor.sh redeploy matrix-d1015d90-c85f-4e84-8ca6-08
./deploy/railway/rollout-service-supervisor.sh verify matrix-d1015d90-c85f-4e84-8ca6-08
```

Continue in the printed order only after health and both registries pass verification. The first deployment uses the default `report` policy: unsafe commands are redacted and reported but are not blocked. Rotate exposed credentials and replace inline values with approved secret references before enabling enforcement:

```bash
export MATRIX_RAILWAY_ROLLOUT_APPROVED=YES
./deploy/railway/rollout-service-supervisor.sh enforce <service>
./deploy/railway/rollout-service-supervisor.sh verify <service>
```

`enforce` refuses to proceed while either registry still contains credential material. It then sets `MATRIX_EXEC_INLINE_SECRET_POLICY=block` and requests a redeploy. Do not promote past a service with PID aliases, legacy unverified PIDs, or an autostart entry that is not identity-verified and running.

Use the read-only fleet audit at any time:

```bash
./deploy/railway/audit-service-supervisors.sh
```
