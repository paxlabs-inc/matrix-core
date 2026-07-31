package controlapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type resourceQuery struct {
	sql string
}

var resourceQueries = map[string]resourceQuery{
	"organization": {sql: `
		SELECT authority_id,latest_version,updated_at,
		       jsonb_build_object('authority_kind',authority_kind)
		FROM workforce_authority_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='organization'
		ORDER BY updated_at DESC,authority_id LIMIT $3 OFFSET $4`},
	"departments": {sql: `
		SELECT department_id,1,created_at,
		       jsonb_build_object(
		           'department_id',department_id,
		           'kind',department_kind,
		           'enabled',enabled,
		           'seat_count',(
		             SELECT COUNT(*) FROM workforce_organization_seats seat
		             WHERE seat.tenant_id=department.tenant_id
		               AND seat.organization_id=department.organization_id
		               AND seat.department_id=department.department_id
		           ))
		FROM workforce_organization_departments department
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,department_id LIMIT $3 OFFSET $4`},
	"seats": {sql: `
		SELECT seat_id,1,created_at,
		       jsonb_build_object(
		           'department_id',department_id,
		           'role',seat_role,
		           'mandate_id',mandate_id,
		           'mandate_version',mandate_version,
		           'active',active)
		FROM workforce_organization_seats
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,seat_id LIMIT $3 OFFSET $4`},
	"work-orders": {sql: `
		SELECT node_id,version,updated_at,
		       jsonb_build_object(
		           'kind',node_kind,'title',title,'state',state,
		           'owner_seat_id',owner_seat_id,
		           'owner_department_id',owner_department_id,
		           'deadline',deadline,'contested',contested)
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2 AND node_kind='goal'
		ORDER BY updated_at DESC,node_id LIMIT $3 OFFSET $4`},
	"graph": {sql: `
		SELECT node_id,version,updated_at,
		       jsonb_build_object(
		           'kind',node_kind,'title',title,'state',state,
		           'owner_seat_id',owner_seat_id,
		           'owner_department_id',owner_department_id,
		           'priority',base_priority,'deadline',deadline,
		           'contested',contested,'terminal_record_id',terminal_record_id)
		FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,node_id LIMIT $3 OFFSET $4`},
	"graph-edges": {sql: `
		SELECT prerequisite_node_id||':'||dependent_node_id||':'||edge_kind,1,created_at,
		       jsonb_build_object(
		           'prerequisite',prerequisite_node_id,
		           'dependent',dependent_node_id,'kind',edge_kind,
		           'expires_at',expires_at,'sla_at',sla_at,
		           'timeout_action',timeout_action)
		FROM workforce_work_edges
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,prerequisite_node_id,dependent_node_id
		LIMIT $3 OFFSET $4`},
	"mail": {sql: `
		SELECT message_id,1,created_at,
		       jsonb_build_object(
		           'thread_id',thread_id,'in_reply_to',in_reply_to,
		           'sender_department_id',sender_department_id,
		           'sender_seat_id',sender_seat_id,'kind',kind,
		           'parent_intent_id',parent_intent_id,'priority',priority,
		           'deadline',deadline,'classification',classification,
		           'binding_state',binding_state,'expires_at',expires_at,
		           'recipients',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'department_id',r.recipient_department_id,
		               'seat_id',r.recipient_seat_id,
		               'kind',r.recipient_kind,'state',r.state,
		               'updated_at',r.updated_at
		             ) ORDER BY r.recipient_kind,r.recipient_seat_id)
		             FROM workforce_mail_recipients r
		             WHERE r.tenant_id=m.tenant_id
		               AND r.organization_id=m.organization_id
		               AND r.message_id=m.message_id
		           ),'[]'::jsonb))
		FROM workforce_mail_messages m
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,message_id LIMIT $3 OFFSET $4`},
	"approvals": {sql: `
		SELECT batch_id,1,created_at,
		       jsonb_build_object(
		           'intent_set_hash',intent_set_hash,
		           'intent_ids',COALESCE((
		             SELECT jsonb_agg(i.intent_id ORDER BY i.intent_id)
		             FROM workforce_approval_batch_intents i
		             WHERE i.tenant_id=b.tenant_id
		               AND i.organization_id=b.organization_id
		               AND i.batch_id=b.batch_id
		           ),'[]'::jsonb),
		           'aggregate_ceiling_microunits',aggregate_ceiling_microunits,
		           'consumed_microunits',consumed_microunits,
		           'expires_at',expires_at,'revoked_at',revoked_at,
		           'owner_id',owner_id,'key_id',key_id,
		           'canonical_hash',canonical_hash)
		FROM workforce_approval_batches b
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,batch_id LIMIT $3 OFFSET $4`},
	"receipts": {sql: `
		SELECT receipt_id,1,created_at,
		       jsonb_build_object(
		           'wake_id',wake_id,'intent_id',intent_id,
		           'disposition',disposition,'content_hash',content_hash)
		FROM workforce_execution_receipts
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,receipt_id LIMIT $3 OFFSET $4`},
	"policies": {sql: `
		SELECT authority_kind||':'||authority_id||':'||version,version,created_at,
		       jsonb_build_object(
		           'authority_kind',authority_kind,'authority_id',authority_id,
		           'owner_id',owner_id,'key_id',key_id,
		           'effective_at',effective_at,'canonical_hash',canonical_hash,
		           'material_change',material_change)
		FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind IN ('policy','mandate')
		ORDER BY created_at DESC,authority_kind,authority_id,version DESC
		LIMIT $3 OFFSET $4`},
	"schedules": {sql: `
		SELECT wake_id,1,updated_at,
		       jsonb_build_object(
		           'schedule_id',schedule_id,'seat_id',seat_id,
		           'trigger_kind',trigger_kind,'reason',reason,'state',state,
		           'scheduled_at',scheduled_at,'completed_at',completed_at,
		           'budget_tasks',budget_tasks,
		           'budget_spend_microunits',budget_spend_microunits)
		FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,wake_id LIMIT $3 OFFSET $4`},
	"incidents": {sql: `
		SELECT incident_id,1,created_at,
		       jsonb_build_object(
		           'kind',kind,'node_ids',node_ids,
		           'explanation',explanation,'resolved_at',resolved_at)
		FROM workforce_work_incidents
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,incident_id LIMIT $3 OFFSET $4`},
	"project-brain": {sql: `
		SELECT record_id,version,verified_at,
		       jsonb_build_object(
		           'project_id',project_id,'workspace_id',workspace_id,
		           'kind',kind,'author_seat_id',author_seat_id,
		           'verifier_seat_id',verifier_seat_id,
		           'source_root',source_root,'graph_generation',graph_generation,
		           'fresh',fresh,'supersedes',supersedes,'corrects',corrects,
		           'canonical_hash',canonical_hash)
		FROM workforce_project_brain_records
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY verified_at DESC,record_id LIMIT $3 OFFSET $4`},
	"corrections": {sql: `
		SELECT correction_id,1,created_at,
		       jsonb_build_object(
		           'status',status,'materially_unsafe',materially_unsafe,
		           'correction_record_id',correction_record_id,
		           'source_record_id',source_record_id,'closed_at',closed_at,
		           'affected',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'record_id',t.affected_record_id,
		               'consumer_seat_id',t.consumer_seat_id,
		               'state',t.state,'materially_unsafe',t.materially_unsafe,
		               'paused',t.paused,'evidence_record_id',t.evidence_record_id,
		               'resolved_at',t.resolved_at
		             ) ORDER BY t.affected_record_id)
		             FROM workforce_correction_targets t
		             WHERE t.tenant_id=c.tenant_id
		               AND t.organization_id=c.organization_id
		               AND t.correction_id=c.correction_id
		           ),'[]'::jsonb))
		FROM workforce_corrections c
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,correction_id LIMIT $3 OFFSET $4`},
	"audit-disagreements": {sql: `
		SELECT epoch_id||':'||original_verdict_id,1,committed_at,
		       jsonb_build_object(
		           'epoch_id',epoch_id,'original_verdict_id',original_verdict_id,
		           'reaudit_verdict_id',reaudit_verdict_id,
		           'original_outcome',original_outcome,
		           'reaudit_outcome',reaudit_outcome,
		           'disagreement',disagreement,
		           'incidents',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'incident_id',i.incident_id,'reason',i.reason,
		               'created_at',i.created_at
		             ) ORDER BY i.created_at,i.incident_id)
		             FROM workforce_cross_audit_incidents i
		             WHERE i.tenant_id=r.tenant_id
		               AND i.organization_id=r.organization_id
		               AND i.epoch_id=r.epoch_id
		               AND i.original_verdict_id=r.original_verdict_id
		           ),'[]'::jsonb))
		FROM workforce_cross_audit_results r
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY committed_at DESC,epoch_id,original_verdict_id LIMIT $3 OFFSET $4`},
	"replay-lineage": {sql: `
		SELECT evidence_id,1,created_at,
		       jsonb_build_object(
		           'wake_id',wake_id,'request_hash',request_hash,
		           'response_hash',response_hash,'replay_retained',replay_retained,
		           'envelope_hash',envelope_hash)
		FROM workforce_model_evidence
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,evidence_id LIMIT $3 OFFSET $4`},
	"effect-status": {sql: `
		SELECT proposal_id,1,updated_at,
		       jsonb_build_object(
		           'intent_id',intent_id,'seat_id',seat_id,
		           'provider',provider,'operation',operation,'state',state,
		           'external_id',external_id,'evidence_hash',evidence_hash,
		           'safe_error_code',safe_error_code)
		FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,proposal_id LIMIT $3 OFFSET $4`},
	"control-versions": {sql: `
		SELECT resource_kind||':'||resource_id,version,updated_at,
		       jsonb_build_object(
		           'resource_kind',resource_kind,'resource_id',resource_id,
		           'command_id',command_id)
		FROM workforce_control_versions
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,resource_kind,resource_id LIMIT $3 OFFSET $4`},
}

func listResource(
	ctx context.Context,
	pool *pgxpool.Pool,
	principal Principal,
	resource, cursor string,
	limit int,
) (ResourcePage, error) {
	query, exists := resourceQueries[resource]
	if !exists {
		return ResourcePage{}, fmt.Errorf("controlapi: unknown resource")
	}
	offset, err := decodePageCursor(resource, cursor)
	if err != nil {
		return ResourcePage{}, err
	}
	rows, err := pool.Query(
		ctx, query.sql, principal.TenantID, principal.OrganizationID, limit, offset,
	)
	if err != nil {
		return ResourcePage{}, err
	}
	defer rows.Close()
	page := ResourcePage{
		SchemaVersion: SchemaVersion, Resource: resource,
		Items: make([]ResourceItem, 0, limit),
	}
	for rows.Next() {
		var item ResourceItem
		var fields []byte
		if err := rows.Scan(&item.ID, &item.Version, &item.UpdatedAt, &fields); err != nil {
			return ResourcePage{}, err
		}
		if err := json.Unmarshal(fields, &item.Fields); err != nil {
			return ResourcePage{}, err
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return ResourcePage{}, err
	}
	if len(page.Items) == limit {
		page.NextCursor = encodePageCursor(resource, offset+uint64(len(page.Items)))
	}
	return page, nil
}

func encodePageCursor(resource string, offset uint64) string {
	value := resource + ":" + strconv.FormatUint(offset, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodePageCursor(resource, value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, fmt.Errorf("controlapi: invalid page cursor")
	}
	prefix, raw, found := strings.Cut(string(decoded), ":")
	if !found || prefix != resource {
		return 0, fmt.Errorf("controlapi: page cursor belongs to another resource")
	}
	offset, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("controlapi: invalid page cursor")
	}
	return offset, nil
}
