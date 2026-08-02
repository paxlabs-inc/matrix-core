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
	"migrations": {sql: `
		SELECT head.migration_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'state',head.state,
		           'from_template_id',migration.from_template_id,
		           'from_template_version',migration.from_template_version,
		           'to_template_id',migration.to_template_id,
		           'to_template_version',migration.to_template_version,
		           'manifest_digest',migration.manifest_digest,
		           'capability_registry_digest',migration.capability_registry_digest,
		           'canonical_hash',migration.canonical_hash,
		           'prepared_at',migration.prepared_at,
		           'activate_not_before',migration.activate_not_before,
		           'expires_at',migration.expires_at)
		FROM workforce_organization_migration_heads head
		JOIN workforce_organization_migrations migration
		  ON migration.tenant_id=head.tenant_id
		 AND migration.organization_id=head.organization_id
		 AND migration.migration_id=head.migration_id
		 AND migration.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.migration_id LIMIT $3 OFFSET $4`},
	"company-authority": {sql: `
		SELECT authority_kind||':'||authority_id,latest_version,updated_at,
		       jsonb_build_object(
		           'authority_kind',authority_kind,
		           'authority_id',authority_id,
		           'organization_schema','workforce.organization.v2')
		FROM workforce_company_authority_heads
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,authority_kind,authority_id LIMIT $3 OFFSET $4`},
	"company-runtime": {sql: `
		SELECT head.config_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'state',head.state,'procedure_id',config.procedure_id,
		           'procedure_version',config.procedure_version,
		           'mission_version',config.mission_version,
		           'constitution_version',config.constitution_version,
		           'capital_envelope_version',config.capital_envelope_version,
		           'issuer_policy_version',config.issuer_policy_version,
		           'effective_at',config.effective_at,'expires_at',config.expires_at,
		           'canonical_hash',config.canonical_hash)
		FROM workforce_company_runtime_heads head
		JOIN workforce_company_runtime_configs config
		  ON config.tenant_id=head.tenant_id AND config.organization_id=head.organization_id
		 AND config.config_id=head.config_id AND config.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.config_id LIMIT $3 OFFSET $4`},
	"company-cycles": {sql: `
		SELECT run.cycle_id,1,run.updated_at,
		       jsonb_build_object(
		           'kind',run.cadence_kind,'due_at',run.due_at,'next_at',run.next_at,
		           'departments',run.departments,'required_capabilities',run.required_capabilities,
		           'independent_audit',run.independent_audit,'state',run.state,
		           'work_order_id',dispatch.work_order_id,'goal_id',dispatch.goal_id,
		           'wake_id',dispatch.wake_id)
		FROM workforce_company_cycle_runs run
		LEFT JOIN workforce_company_cycle_dispatches dispatch
		  ON dispatch.tenant_id=run.tenant_id AND dispatch.organization_id=run.organization_id
		 AND dispatch.cycle_id=run.cycle_id
		WHERE run.tenant_id=$1 AND run.organization_id=$2
		ORDER BY run.updated_at DESC,run.cycle_id LIMIT $3 OFFSET $4`},
	"company-funding": {sql: `
		SELECT funding_id,GREATEST(lifecycle_version,1),updated_at,
		       jsonb_build_object(
		           'opportunity_id',opportunity_id,'initiative_id',initiative_id,
		           'decision_id',decision_id,'decision_hash',decision_hash,
		           'lifecycle_version',lifecycle_version,'plan_id',plan_id,
		           'plan_version',plan_version,'state',state,'error_code',error_code)
		FROM workforce_company_funding_runs
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,funding_id LIMIT $3 OFFSET $4`},
	"company-state": {sql: `
		SELECT record_id,latest_version,updated_at,
		       jsonb_build_object(
		           'kind',kind,'domain',domain,'state',state,
		           'expires_at',expires_at,'content_hash',latest_content_hash,
		           'materially_contaminated',EXISTS (
		             SELECT 1 FROM workforce_company_state_contamination contamination
		             WHERE contamination.tenant_id=head.tenant_id
		               AND contamination.organization_id=head.organization_id
		               AND contamination.affected_record_id=head.record_id
		               AND contamination.affected_version=head.latest_version
		               AND contamination.state='open'
		               AND contamination.materially_unsafe
		           ))
		FROM workforce_company_state_heads head
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,record_id LIMIT $3 OFFSET $4`},
	"opportunities": {sql: `
		SELECT head.opportunity_id,head.current_version,head.updated_at,
		       jsonb_build_object(
		           'canonical_identity',head.canonical_identity,'state',head.state,
		           'source_kind',record.source_kind,'author_seat_id',record.author_seat_id,
		           'submitted_at',record.submitted_at,'expires_at',record.expires_at,
		           'canonical_hash',head.canonical_hash)
		FROM workforce_opportunity_heads head
		JOIN workforce_opportunities record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.opportunity_id=head.opportunity_id AND record.version=head.current_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.opportunity_id LIMIT $3 OFFSET $4`},
	"portfolio-decisions": {sql: `
		SELECT decision_id,procedure_version,created_at,
		       jsonb_build_object(
		           'opportunity_id',opportunity_id,'procedure_id',procedure_id,
		           'decision',decision,'score_bps',score_bps,
		           'capital_impact_microunits',capital_impact_microunits,
		           'risk_impact_microunits',risk_impact_microunits,
		           'next_review_at',next_review_at,'canonical_hash',canonical_hash)
		FROM workforce_portfolio_decisions
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,decision_id LIMIT $3 OFFSET $4`},
	"portfolio-allocations": {sql: `
		SELECT initiative_id,1,updated_at,
		       jsonb_build_object(
		           'opportunity_id',opportunity_id,'decision_id',decision_id,
		           'capital_microunits',capital_microunits,
		           'risk_microunits',risk_microunits,'state',state,
		           'created_at',created_at)
		FROM workforce_portfolio_allocations
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,initiative_id LIMIT $3 OFFSET $4`},
	"company-cadences": {sql: `
		SELECT cadence_id,version,updated_at,
		       jsonb_build_object(
		           'kind',kind,'interval_seconds',interval_seconds,
		           'next_due_at',next_due_at,'effective_at',effective_at,
		           'expires_at',expires_at,'state',state)
		FROM workforce_company_cadences
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,cadence_id LIMIT $3 OFFSET $4`},
	"company-lifecycle": {sql: `
		SELECT head.initiative_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'state',head.state,'resume_state',head.resume_state,
		           'opportunity_id',initiative.opportunity_id,
		           'portfolio_id',initiative.portfolio_id,
		           'checkpoint_hash',head.checkpoint_hash,
		           'last_transition_id',head.last_transition_id,
		           'last_receipt_id',head.last_receipt_id,
		           'last_receipt_hash',head.last_receipt_hash,
		           'terminated_at',head.terminated_at)
		FROM workforce_lifecycle_heads head
		JOIN workforce_lifecycle_initiatives initiative
		  ON initiative.tenant_id=head.tenant_id
		 AND initiative.organization_id=head.organization_id
		 AND initiative.initiative_id=head.initiative_id
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.initiative_id LIMIT $3 OFFSET $4`},
	"initiatives": {sql: `
		SELECT initiative.initiative_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'opportunity_id',initiative.opportunity_id,
		           'portfolio_id',initiative.portfolio_id,
		           'state',head.state,'resume_state',head.resume_state,
		           'checkpoint_hash',head.checkpoint_hash,
		           'last_transition_id',head.last_transition_id,
		           'last_receipt_id',head.last_receipt_id,
		           'last_receipt_hash',head.last_receipt_hash,
		           'created_at',initiative.created_at,
		           'terminated_at',head.terminated_at)
		FROM workforce_lifecycle_initiatives initiative
		JOIN workforce_lifecycle_heads head
		  ON head.tenant_id=initiative.tenant_id
		 AND head.organization_id=initiative.organization_id
		 AND head.initiative_id=initiative.initiative_id
		WHERE initiative.tenant_id=$1 AND initiative.organization_id=$2
		ORDER BY head.updated_at DESC,initiative.initiative_id LIMIT $3 OFFSET $4`},
	"lifecycle-transitions": {sql: `
		SELECT transition_id,sequence,created_at,
		       jsonb_build_object(
		           'initiative_id',initiative_id,'from_state',from_state,
		           'to_state',to_state,'decision',decision,
		           'request_hash',request_hash,'checkpoint_hash',checkpoint_hash,
		           'receipt_id',receipt_id,'receipt_hash',receipt_hash)
		FROM workforce_lifecycle_transition_journal
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,initiative_id,sequence DESC LIMIT $3 OFFSET $4`},
	"initiative-plans": {sql: `
		SELECT head.plan_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'initiative_id',head.initiative_id,'state',head.state,
		           'initiative_version',plan.initiative_version,
		           'blueprint_id',plan.blueprint_id,
		           'blueprint_version',plan.blueprint_version,
		           'portfolio_decision_id',plan.portfolio_decision_id,
		           'capital_allocation_id',plan.capital_allocation_id,
		           'capability_plan_id',plan.capability_plan_id,
		           'capital_microunits',plan.capital_microunits,
		           'risk_microunits',plan.risk_microunits,
		           'canonical_hash',plan.canonical_hash)
		FROM workforce_company_initiative_plan_heads head
		JOIN workforce_company_initiative_plans plan
		  ON plan.tenant_id=head.tenant_id AND plan.organization_id=head.organization_id
		 AND plan.initiative_id=head.initiative_id AND plan.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.plan_id LIMIT $3 OFFSET $4`},
	"company-work-orders": {sql: `
		SELECT company.work_order_id,company.plan_version,company.created_at,
		       jsonb_build_object(
		           'initiative_id',company.initiative_id,'plan_node_id',company.plan_node_id,
		           'controller_id',company.controller_id,'issuer_kind',company.issuer_kind,
		           'issuer_policy_version',company.issuer_policy_version,
		           'work_order_class',company.work_order_class,'issue_identity',company.issue_identity,
		           'capital_microunits',company.capital_microunits,
		           'risk_microunits',company.risk_microunits,
		           'canonical_hash',company.canonical_hash,'deadline',company.deadline,
		           'goal_id',dispatch.goal_id,'wake_id',dispatch.wake_id,
		           'execution_state',graph.state)
		FROM workforce_company_work_orders company
		LEFT JOIN workforce_company_work_order_dispatches dispatch
		  ON dispatch.tenant_id=company.tenant_id
		 AND dispatch.organization_id=company.organization_id
		 AND dispatch.work_order_id=company.work_order_id
		LEFT JOIN workforce_work_nodes graph
		  ON graph.tenant_id=dispatch.tenant_id AND graph.organization_id=dispatch.organization_id
		 AND graph.node_id=dispatch.goal_id
		WHERE company.tenant_id=$1 AND company.organization_id=$2
		ORDER BY company.created_at DESC,company.work_order_id LIMIT $3 OFFSET $4`},
	"company-outcome-gates": {sql: `
		SELECT gate_id,plan_version,created_at,
		       jsonb_build_object(
		           'initiative_id',initiative_id,'node_id',node_id,
		           'predicate_id',predicate_id,'state',state,'expires_at',expires_at)
		FROM workforce_company_outcome_gates
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,initiative_id,gate_id LIMIT $3 OFFSET $4`},
	"portfolio-incidents": {sql: `
		SELECT incident_id,1,created_at,
		       jsonb_build_object(
		           'kind',kind,'opportunity_id',opportunity_id,
		           'detail',detail,'state',state,'resolved_at',resolved_at)
		FROM workforce_portfolio_incidents
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,incident_id LIMIT $3 OFFSET $4`},
	"executive-decisions": {sql: `
		SELECT decision.decision_id,decision.policy_version,decision.created_at,
		       jsonb_build_object(
		           'request_id',decision.request_id,'outcome',decision.outcome,
		           'action',decision.action,'policy_id',decision.policy_id,
		           'policy_version',decision.policy_version,
		           'authority_clause',decision.clause_id,
		           'capital_microunits',decision.capital_microunits,
		           'exposure_microunits',decision.exposure_microunits,
		           'rolling_capital_microunits',decision.rolling_capital_microunits,
		           'rolling_exposure_microunits',decision.rolling_exposure_microunits,
		           'canonical_hash',decision.canonical_hash,
		           'authorized_until',decision.authorized_until,
		           'next_review_at',decision.next_review_at,
		           'reviews',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'review_id',review.review_id,'kind',review.kind,
		               'outcome',review.outcome,'reviewer_seat_id',review.reviewer_seat_id,
		               'canonical_hash',review.canonical_hash,
		               'reviewed_at',review.reviewed_at,'expires_at',review.expires_at
		             ) ORDER BY review.kind,review.review_id)
		             FROM workforce_executive_reviews review
		             WHERE review.tenant_id=decision.tenant_id
		               AND review.organization_id=decision.organization_id
		               AND review.request_id=decision.request_id
		           ),'[]'::jsonb))
		FROM workforce_executive_decisions decision
		WHERE decision.tenant_id=$1 AND decision.organization_id=$2
		ORDER BY decision.created_at DESC,decision.decision_id LIMIT $3 OFFSET $4`},
	"founder-decision-requests": {sql: `
		SELECT founder_request_id,1,created_at,
		       jsonb_build_object(
		           'request_id',request_id,'reserved_kind',reserved_kind,
		           'state',state,'canonical_hash',canonical_hash,
		           'expires_at',expires_at)
		FROM workforce_founder_decision_requests
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,founder_request_id LIMIT $3 OFFSET $4`},
	"squads": {sql: `
		SELECT squad.assignment_id,1,squad.created_at,
		       jsonb_build_object(
		           'initiative_id',squad.initiative_id,
		           'lifecycle_stage',squad.lifecycle_stage,
		           'template_id',squad.template_id,
		           'template_version',squad.template_version,
		           'template_digest',squad.template_digest,
		           'requirement_digest',squad.requirement_digest,
		           'graph_scopes',squad.graph_scopes,
		           'conflict_domains',squad.conflict_domains,
		           'member_count',squad.member_count,
		           'authority_effect',squad.authority_effect,
		           'canonical_hash',squad.canonical_hash,
		           'issued_at',squad.issued_at,'expires_at',squad.expires_at,
		           'state',CASE
		             WHEN revocation.assignment_id IS NOT NULL THEN 'revoked'
		             WHEN squad.expires_at<=CURRENT_TIMESTAMP THEN 'expired'
		             ELSE 'active'
		           END,
		           'members',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'seat_id',member.seat_id,
		               'department_id',member.department_id,
		               'seat_role',member.seat_role,
		               'mandate_id',member.mandate_id,
		               'mandate_version',member.mandate_version,
		               'independence_domain',member.independence_domain,
		               'need_ids',member.need_ids,
		               'model_calls',member.model_calls,
		               'tool_calls',member.tool_calls,
		               'effect_dispatches',member.effect_dispatches,
		               'cost_minor',member.cost_minor,
		               'currency',member.currency
		             ) ORDER BY member.department_id,member.seat_role,member.seat_id)
		             FROM workforce_squad_assignment_members member
		             WHERE member.tenant_id=squad.tenant_id
		               AND member.organization_id=squad.organization_id
		               AND member.assignment_id=squad.assignment_id
		           ),'[]'::jsonb))
		FROM workforce_squad_assignments squad
		LEFT JOIN workforce_squad_assignment_revocations revocation
		  ON revocation.tenant_id=squad.tenant_id
		 AND revocation.organization_id=squad.organization_id
		 AND revocation.assignment_id=squad.assignment_id
		WHERE squad.tenant_id=$1 AND squad.organization_id=$2
		ORDER BY squad.created_at DESC,squad.assignment_id LIMIT $3 OFFSET $4`},
	"product-checkpoints": {sql: `
		SELECT head.initiative_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'checkpoint_id',head.checkpoint_id,'phase',head.phase,
		           'handoff_id',checkpoint.handoff_id,
		           'project_id',checkpoint.project_id,
		           'workspace_id',checkpoint.workspace_id,
		           'canonical_hash',checkpoint.canonical_hash)
		FROM workforce_product_capability_checkpoint_heads head
		JOIN workforce_product_capability_checkpoints checkpoint
		  ON checkpoint.tenant_id=head.tenant_id
		 AND checkpoint.organization_id=head.organization_id
		 AND checkpoint.initiative_id=head.initiative_id
		 AND checkpoint.checkpoint_id=head.checkpoint_id
		 AND checkpoint.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.initiative_id LIMIT $3 OFFSET $4`},
	"product-executions": {sql: `
		SELECT execution.execution_id,execution.version,execution.updated_at,
		       jsonb_build_object(
		         'initiative_id',execution.initiative_id,
		         'plan_id',execution.plan_id,'plan_version',execution.plan_version,
		         'squad_assignment_id',execution.squad_assignment_id,
		         'project_id',execution.project_id,'workspace_id',execution.workspace_id,
		         'phase',execution.phase,
		         'product_record_id',execution.product_record_id,
		         'engineering_record_id',execution.engineering_record_id,
		         'deployment_effect_id',execution.deployment_effect_id,
		         'launch_transition_id',execution.launch_transition_id,
		         'checkpoint_version',execution.checkpoint_version,
		         'created_at',execution.created_at,
		         'stages',COALESCE((
		           SELECT jsonb_agg(jsonb_build_object(
		             'stage',binding.stage,'plan_node_id',binding.plan_node_id,
		             'work_order_id',binding.work_order_id,'need_id',binding.need_id,
		             'seat_id',binding.seat_id,'department_id',binding.department_id,
		             'seat_role',binding.seat_role,'mandate_id',binding.mandate_id,
		             'mandate_version',binding.mandate_version,
		             'goal_id',binding.goal_id,'intent_id',binding.intent_id,
		             'wake_id',binding.wake_id
		           ) ORDER BY CASE binding.stage
		             WHEN 'product' THEN 1 WHEN 'design' THEN 2 WHEN 'build' THEN 3
		             WHEN 'verification' THEN 4 WHEN 'deployment' THEN 5 ELSE 6 END)
		           FROM workforce_product_execution_stage_bindings binding
		           WHERE binding.tenant_id=execution.tenant_id
		             AND binding.organization_id=execution.organization_id
		             AND binding.execution_id=execution.execution_id
		         ),'[]'::jsonb),
		         'receipts',COALESCE((
		           SELECT jsonb_agg(jsonb_build_object(
		             'stage',receipt.stage,'receipt_id',receipt.receipt_id,
		             'receipt_hash',receipt.receipt_hash,'verdict_id',receipt.verdict_id,
		             'seat_id',receipt.seat_id,'intent_id',receipt.intent_id,
		             'accepted_at',receipt.accepted_at
		           ) ORDER BY receipt.accepted_at,receipt.stage)
		           FROM workforce_product_execution_stage_receipts receipt
		           WHERE receipt.tenant_id=execution.tenant_id
		             AND receipt.organization_id=execution.organization_id
		             AND receipt.execution_id=execution.execution_id
		         ),'[]'::jsonb),
		         'effects',COALESCE((
		           SELECT jsonb_agg(jsonb_build_object(
		             'effect_id',effect.effect_id,'proposal_id',effect.proposal_id,
		             'operation',effect.operation,'state',effect.state,
		             'external_id',effect.external_id,'evidence_hash',effect.evidence_hash,
		             'prepared_at',effect.prepared_at,'reconciled_at',effect.reconciled_at
		           ) ORDER BY effect.prepared_at,effect.effect_id)
		           FROM workforce_product_execution_effects effect
		           WHERE effect.tenant_id=execution.tenant_id
		             AND effect.organization_id=execution.organization_id
		             AND effect.execution_id=execution.execution_id
		         ),'[]'::jsonb))
		FROM workforce_product_executions execution
		WHERE execution.tenant_id=$1 AND execution.organization_id=$2
		ORDER BY execution.updated_at DESC,execution.execution_id LIMIT $3 OFFSET $4`},
	"product-execution-events": {sql: `
		SELECT execution_id||':'||sequence,sequence,created_at,
		       jsonb_build_object(
		         'execution_id',execution_id,'phase',phase,'event_kind',event_kind,
		         'stage',stage,'source_id',source_id,'content_hash',content_hash)
		FROM workforce_product_execution_events
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,execution_id,sequence DESC LIMIT $3 OFFSET $4`},
	"product-gate-evidence": {sql: `
		SELECT record_id||':'||evidence_id||':'||evidence_kind,record_version,created_at,
		       jsonb_build_object(
		           'record_id',record_id,'initiative_id',initiative_id,
		           'evidence_id',evidence_id,'evidence_kind',evidence_kind,
		           'evidence_hash',evidence_hash,'fresh_until',fresh_until,
		           'verdict_id',verdict_id,'verdict_hash',verdict_hash,
		           'fresh',fresh_until>CURRENT_TIMESTAMP)
		FROM workforce_product_capability_gate_bindings
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,record_id,evidence_kind,evidence_id LIMIT $3 OFFSET $4`},
	"organization": {sql: `
		SELECT authority_id,latest_version,updated_at,
		       jsonb_build_object('authority_kind',authority_kind)
		FROM workforce_authority_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND authority_kind='organization'
		ORDER BY updated_at DESC,authority_id LIMIT $3 OFFSET $4`},
	"capabilities": {sql: `
		SELECT head.capability_id,head.latest_version,head.updated_at,
		       jsonb_build_object(
		           'capability_kind',definition.capability_kind,
		           'canonical_hash',definition.canonical_hash,
		           'signature_key_id',definition.signature_key_id,
		           'effective_at',definition.effective_at,
		           'expires_at',definition.expires_at)
		FROM workforce_capability_definition_heads head
		JOIN workforce_capability_definitions definition
		  ON definition.tenant_id=head.tenant_id
		 AND definition.organization_id=head.organization_id
		 AND definition.capability_id=head.capability_id
		 AND definition.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.capability_id LIMIT $3 OFFSET $4`},
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
	"organization-mandates": {sql: `
		SELECT mandate_id,version,created_at,
		       jsonb_build_object(
		           'seat_id',seat_id,'department_id',department_id,
		           'seat_role',seat_role,'mandate_origin',mandate_origin,
		           'canonical_hash',canonical_hash,
		           'signature_key_id',signature_key_id,
		           'effective_at',effective_at,'expires_at',expires_at)
		FROM workforce_organization_seat_mandates
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,mandate_id,version DESC LIMIT $3 OFFSET $4`},
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
	"customer-connections": {sql: `
		SELECT head.connection_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'adapter_name',connection.adapter_name,
		           'family',connection.family,'account_id',connection.account_id,
		           'identity_id',connection.identity_id,'state',head.state,
		           'canonical_hash',head.canonical_hash,
		           'effective_at',head.effective_at,'expires_at',head.expires_at,
		           'revoked_at',head.revoked_at)
		FROM workforce_customer_connection_heads head
		JOIN workforce_customer_connections connection
		  ON connection.tenant_id=head.tenant_id
		 AND connection.organization_id=head.organization_id
		 AND connection.connection_id=head.connection_id
		 AND connection.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.connection_id LIMIT $3 OFFSET $4`},
	"customers": {sql: `
		SELECT customer_id,version,updated_at,
		       jsonb_build_object(
		           'connection_id',connection_id,
		           'connection_version',connection_version,
		           'recipient_hash',recipient_hash,'state',state,
		           'canonical_hash',canonical_hash,
		           'effective_at',effective_at,'expires_at',expires_at)
		FROM workforce_customer_scope_heads
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,customer_id LIMIT $3 OFFSET $4`},
	"customer-consents": {sql: `
		SELECT consent_id,version,updated_at,
		       jsonb_build_object(
		           'connection_id',connection_id,'customer_id',customer_id,
		           'channel',channel,'purpose',purpose,
		           'jurisdiction',jurisdiction,'basis',basis,'state',state,
		           'canonical_hash',canonical_hash,
		           'effective_at',effective_at,'expires_at',expires_at)
		FROM workforce_customer_consent_heads
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,consent_id LIMIT $3 OFFSET $4`},
	"customer-operations": {sql: `
		SELECT observation_id,1,captured_at,
		       jsonb_build_object(
		           'connection_id',connection_id,'operation',operation,
		           'customer_id',customer_id,'external_id',external_id,
		           'external_state',external_state,'authority',authority,
		           'canonical_hash',canonical_hash,
		           'provider_observed_at',provider_observed_at)
		FROM workforce_customer_observations
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY captured_at DESC,observation_id LIMIT $3 OFFSET $4`},
	"customer-incidents": {sql: `
		SELECT incident_id,1,created_at,
		       jsonb_build_object(
		           'connection_id',connection_id,'operation',operation,
		           'kind',kind,'safe_code',safe_code,'state',state,
		           'resolved_at',resolved_at)
		FROM workforce_customer_incidents
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,incident_id LIMIT $3 OFFSET $4`},
	"financial-connections": {sql: `
		SELECT head.connection_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'adapter_name',connection.adapter_name,
		           'family',connection.family,'account_id',connection.account_id,
		           'base_currency',connection.base_currency,
		           'provider_contract_kind',connection.provider_contract_kind,
		           'network_id',connection.network_id,'chain_id',connection.chain_id,
		           'settlement_contract',connection.settlement_contract,
		           'contract_version',connection.contract_version,
		           'required_confirmations',connection.required_confirmations,
		           'state',head.state,'canonical_hash',head.canonical_hash,
		           'effective_at',head.effective_at,'expires_at',head.expires_at,
		           'revoked_at',head.revoked_at)
		FROM workforce_financial_connection_heads head
		JOIN workforce_financial_connections connection
		  ON connection.tenant_id=head.tenant_id
		 AND connection.organization_id=head.organization_id
		 AND connection.connection_id=head.connection_id
		 AND connection.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.connection_id LIMIT $3 OFFSET $4`},
	"financial-risk": {sql: `
		SELECT head.connection_id||':'||head.snapshot_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'connection_id',head.connection_id,
		           'connection_version',head.connection_version,
		           'snapshot_id',head.snapshot_id,
		           'resource_version',head.resource_version,
		           'base_currency',connection.base_currency,
		           'source_kind',snapshot.source_kind,'source_id',snapshot.source_id,
		           'total_capital_microunits',snapshot.total_capital_microunits,
		           'available_liquidity_microunits',snapshot.available_liquidity_microunits,
		           'gross_exposure_microunits',snapshot.gross_exposure_microunits,
		           'drawdown_microunits',snapshot.drawdown_microunits,
		           'runway_microunits',snapshot.runway_microunits,
		           'canonical_hash',head.canonical_hash,
		           'observed_at',head.observed_at,'expires_at',head.expires_at,
		           'fresh',head.expires_at>CURRENT_TIMESTAMP)
		FROM workforce_financial_risk_heads head
		JOIN workforce_financial_risk_snapshots snapshot
		  ON snapshot.tenant_id=head.tenant_id
		 AND snapshot.organization_id=head.organization_id
		 AND snapshot.connection_id=head.connection_id
		 AND snapshot.connection_version=head.connection_version
		 AND snapshot.snapshot_id=head.snapshot_id
		 AND snapshot.version=head.version
		JOIN workforce_financial_connections connection
		  ON connection.tenant_id=head.tenant_id
		 AND connection.organization_id=head.organization_id
		 AND connection.connection_id=head.connection_id
		 AND connection.version=head.connection_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.connection_id LIMIT $3 OFFSET $4`},
	"financial-observations": {sql: `
		SELECT observation_id,1,created_at,
		       jsonb_build_object(
		           'reservation_id',reservation_id,'connection_id',connection_id,
		           'connection_version',connection_version,'operation',operation,
		           'external_id',external_id,'financial_state',financial_state,
		           'authority',authority,'reconciled',reconciled,
		           'economic_truth',economic_truth,'canonical_hash',canonical_hash,
		           'provider_observed_at',provider_observed_at,
		           'captured_at',captured_at)
		FROM workforce_financial_observations
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,observation_id LIMIT $3 OFFSET $4`},
	"financial-accounting": {sql: `
		SELECT entry_id,1,created_at,
		       jsonb_build_object(
		           'observation_id',observation_id,'reservation_id',reservation_id,
		           'initiative_id',initiative_id,'account_id',account_id,
		           'side',side,'currency',currency,'microunits',microunits,
		           'valuation_time',valuation_time,'methodology_id',methodology_id,
		           'evidence_hash',evidence_hash)
		FROM workforce_financial_accounting_entries
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,entry_id LIMIT $3 OFFSET $4`},
	"financial-incidents": {sql: `
		SELECT incident_id,1,created_at,
		       jsonb_build_object(
		           'reservation_id',reservation_id,'connection_id',connection_id,
		           'operation',operation,'kind',kind,'safe_code',safe_code,
		           'state',state,'resolved_at',resolved_at)
		FROM workforce_financial_incidents
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,incident_id LIMIT $3 OFFSET $4`},
	"financial-ambiguities": {sql: `
		WITH ambiguity AS (
		  SELECT reservation_id AS id,1::bigint AS version,updated_at,
		         jsonb_build_object(
		           'source','financial_reservation','reservation_id',reservation_id,
		           'connection_id',connection_id,'operation',operation,
		           'initiative_id',initiative_id,'asset',asset,'venue',venue,
		           'counterparty',counterparty,'destination_hash',destination_hash,
		           'notional_microunits',notional_microunits,
		           'exposure_increase_microunits',exposure_increase_microunits,
		           'maximum_loss_microunits',maximum_loss_microunits,
		           'fee_ceiling_microunits',fee_ceiling_microunits,
		           'state',state) AS fields
		  FROM workforce_financial_reservations
		  WHERE tenant_id=$1 AND organization_id=$2 AND state='ambiguous'
		  UNION ALL
		  SELECT freeze_id AS id,1::bigint AS version,created_at AS updated_at,
		         jsonb_build_object(
		           'source','financial_scope_freeze','reservation_id',reservation_id,
		           'scope_kind',scope_kind,'scope_key',scope_key,
		           'reason_code',reason_code,'state',state) AS fields
		  FROM workforce_financial_scope_freezes
		  WHERE tenant_id=$1 AND organization_id=$2 AND state IN ('open','escalated')
		)
		SELECT id,version,updated_at,fields FROM ambiguity
		ORDER BY updated_at DESC,id LIMIT $3 OFFSET $4`},
	"business-metrics": {sql: `
		SELECT head.metric_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'initiative_id',head.initiative_id,
		           'outcome_kind',head.outcome_kind,
		           'definition_hash',head.definition_hash,
		           'aggregation',definition.aggregation,'unit',definition.unit,
		           'scale',definition.scale,'author_seat_id',definition.author_seat_id,
		           'registered_at',definition.registered_at,
		           'effective_at',head.effective_at,'expires_at',head.expires_at,
		           'fresh',head.expires_at>CURRENT_TIMESTAMP)
		FROM workforce_business_metric_heads head
		JOIN workforce_business_metric_definitions definition
		  ON definition.tenant_id=head.tenant_id
		 AND definition.organization_id=head.organization_id
		 AND definition.metric_id=head.metric_id
		 AND definition.version=head.version
		 AND definition.definition_hash=head.definition_hash
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.metric_id LIMIT $3 OFFSET $4`},
	"business-observations": {sql: `
		SELECT observation.observation_id,observation.metric_version,observation.captured_at,
		       jsonb_build_object(
		           'initiative_id',observation.initiative_id,
		           'metric_id',observation.metric_id,
		           'metric_version',observation.metric_version,
		           'definition_hash',observation.definition_hash,
		           'outcome_kind',observation.outcome_kind,
		           'status',observation.status,'source_family',observation.source_family,
		           'source_event_id',observation.source_event_id,
		           'source_hash',observation.source_hash,
		           'value_micros',observation.value_micros,
		           'numerator_micros',observation.numerator_micros,
		           'denominator',observation.denominator,
		           'unit',observation.unit,'scale',observation.scale,
		           'uncertainty_bps',observation.uncertainty_bps,
		           'content_hash',observation.content_hash,
		           'observed_at',observation.observed_at,
		           'fresh_until',observation.fresh_until,
		           'supersedes',observation.supersedes,
		           'contaminated',EXISTS (
		             SELECT 1 FROM workforce_business_contamination contamination
		             WHERE contamination.tenant_id=observation.tenant_id
		               AND contamination.organization_id=observation.organization_id
		               AND contamination.affected_kind='observation'
		               AND contamination.affected_id=observation.observation_id
		               AND contamination.affected_hash=observation.content_hash
		               AND contamination.state='open' AND contamination.material
		           ),
		           'fresh',observation.fresh_until>CURRENT_TIMESTAMP,
		           'sources',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'source_role',source.source_role,'source_family',source.source_family,
		               'authority',source.authority,'record_id',source.record_id,
		               'event_id',source.event_id,'source_hash',source.source_hash,
		               'provider',source.provider,'account_ref',source.account_ref,
		               'object_ref',source.object_ref,'source_state',source.source_state,
		               'observed_at',source.observed_at
		             ) ORDER BY source.source_role,source.source_family,source.event_id)
		             FROM workforce_business_observation_sources source
		             WHERE source.tenant_id=observation.tenant_id
		               AND source.organization_id=observation.organization_id
		               AND source.observation_id=observation.observation_id
		           ),'[]'::jsonb))
		FROM workforce_business_observations observation
		WHERE observation.tenant_id=$1 AND observation.organization_id=$2
		ORDER BY observation.captured_at DESC,observation.observation_id LIMIT $3 OFFSET $4`},
	"business-outcomes": {sql: `
		SELECT head.outcome_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'chain_id',head.chain_id,'initiative_id',head.initiative_id,
		           'metric_id',head.metric_id,'metric_version',outcome.metric_version,
		           'definition_hash',outcome.definition_hash,
		           'outcome_kind',head.outcome_kind,
		           'threshold_result',outcome.threshold_result,
		           'value_micros',outcome.value_micros,
		           'numerator_micros',outcome.numerator_micros,
		           'denominator',outcome.denominator,
		           'unit',outcome.unit,'scale',outcome.scale,
		           'record_hash',head.record_hash,
		           'author_seat_id',outcome.author_seat_id,
		           'verifier_seat_id',outcome.verifier_seat_id,
		           'derived_at',outcome.derived_at,
		           'fresh_until',head.fresh_until,
		           'verified_at',outcome.verified_at,
		           'review_expires_at',outcome.review_expires_at,
		           'supersedes',outcome.supersedes,
		           'fresh',head.fresh_until>CURRENT_TIMESTAMP
		             AND outcome.review_expires_at>CURRENT_TIMESTAMP,
		           'contaminated',EXISTS (
		             SELECT 1 FROM workforce_business_contamination contamination
		             WHERE contamination.tenant_id=outcome.tenant_id
		               AND contamination.organization_id=outcome.organization_id
		               AND contamination.affected_kind='outcome'
		               AND contamination.affected_id=outcome.outcome_id
		               AND contamination.affected_hash=outcome.record_hash
		               AND contamination.state='open' AND contamination.material
		           ),
		           'observation_ids',COALESCE((
		             SELECT jsonb_agg(binding.observation_id ORDER BY binding.observation_id)
		             FROM workforce_business_outcome_observations binding
		             WHERE binding.tenant_id=outcome.tenant_id
		               AND binding.organization_id=outcome.organization_id
		               AND binding.outcome_id=outcome.outcome_id
		           ),'[]'::jsonb))
		FROM workforce_business_outcome_heads head
		JOIN workforce_business_outcomes outcome
		  ON outcome.tenant_id=head.tenant_id
		 AND outcome.organization_id=head.organization_id
		 AND outcome.outcome_id=head.outcome_id
		 AND outcome.record_hash=head.record_hash
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.outcome_id LIMIT $3 OFFSET $4`},
	"business-gates": {sql: `
		SELECT head.gate_id,1,head.updated_at,
		       jsonb_build_object(
		           'state',head.state,'decision_hash',head.decision_hash,
		           'purpose',decision.purpose,'initiative_id',decision.initiative_id,
		           'outcome_id',decision.outcome_id,
		           'outcome_hash',decision.outcome_hash,
		           'metric_id',decision.metric_id,
		           'metric_version',decision.metric_version,
		           'definition_hash',decision.definition_hash,
		           'outcome_kind',decision.outcome_kind,
		           'evaluated_at',head.evaluated_at,
		           'contaminated',EXISTS (
		             SELECT 1 FROM workforce_business_contamination contamination
		             WHERE contamination.tenant_id=decision.tenant_id
		               AND contamination.organization_id=decision.organization_id
		               AND contamination.affected_kind='gate'
		               AND contamination.affected_id=decision.gate_id
		               AND contamination.affected_hash=decision.decision_hash
		               AND contamination.state='open' AND contamination.material
		           ))
		FROM workforce_business_gate_heads head
		JOIN workforce_business_gate_decisions decision
		  ON decision.tenant_id=head.tenant_id
		 AND decision.organization_id=head.organization_id
		 AND decision.gate_id=head.gate_id
		 AND decision.decision_hash=head.decision_hash
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.gate_id LIMIT $3 OFFSET $4`},
	"business-lineage": {sql: `
		SELECT edge_id,1,created_at,
		       jsonb_build_object(
		           'initiative_id',initiative_id,'source_kind',source_kind,
		           'source_id',source_id,'source_hash',source_hash,
		           'consumer_kind',consumer_kind,'consumer_id',consumer_id,
		           'consumer_hash',consumer_hash,'relation',relation,
		           'material',material,'canonical_hash',canonical_hash)
		FROM workforce_business_lineage_edges
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,edge_id LIMIT $3 OFFSET $4`},
	"business-corrections": {sql: `
		SELECT correction.correction_id,1,correction.created_at,
		       jsonb_build_object(
		           'initiative_id',correction.initiative_id,
		           'target_kind',correction.target_kind,
		           'target_id',correction.target_id,
		           'target_hash',correction.target_hash,
		           'replacement_kind',correction.replacement_kind,
		           'replacement_id',correction.replacement_id,
		           'replacement_hash',correction.replacement_hash,
		           'material',correction.material,
		           'content_hash',correction.content_hash,
		           'canonical_hash',correction.canonical_hash,
		           'effective_at',correction.effective_at,
		           'affected',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'affected_kind',contamination.affected_kind,
		               'affected_id',contamination.affected_id,
		               'affected_hash',contamination.affected_hash,
		               'derivation_depth',contamination.derivation_depth,
		               'material',contamination.material,'state',contamination.state,
		               'contaminated_at',contamination.contaminated_at,
		               'resolved_at',contamination.resolved_at,
		               'resolution_id',contamination.resolution_id
		             ) ORDER BY contamination.derivation_depth,contamination.affected_kind,
		                        contamination.affected_id)
		             FROM workforce_business_contamination contamination
		             WHERE contamination.tenant_id=correction.tenant_id
		               AND contamination.organization_id=correction.organization_id
		               AND contamination.correction_id=correction.correction_id
		           ),'[]'::jsonb))
		FROM workforce_business_corrections correction
		WHERE correction.tenant_id=$1 AND correction.organization_id=$2
		ORDER BY correction.created_at DESC,correction.correction_id LIMIT $3 OFFSET $4`},
	"commercial-checkpoints": {sql: `
		SELECT head.initiative_id||':'||head.workflow_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'checkpoint_id',head.checkpoint_id,'phase',head.phase,
		           'initiative_id',checkpoint.initiative_id,
		           'skill_id',checkpoint.skill_id,
		           'record_chain_id',checkpoint.record_chain_id,
		           'source_generation',checkpoint.source_generation,
		           'canonical_hash',checkpoint.canonical_hash,
		           'updated_at',checkpoint.updated_at)
		FROM workforce_commercial_checkpoint_heads head
		JOIN workforce_commercial_checkpoints checkpoint
		  ON checkpoint.tenant_id=head.tenant_id
		 AND checkpoint.organization_id=head.organization_id
		 AND checkpoint.initiative_id=head.initiative_id
		 AND checkpoint.workflow_id=head.workflow_id
		 AND checkpoint.checkpoint_id=head.checkpoint_id
		 AND checkpoint.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.workflow_id LIMIT $3 OFFSET $4`},
	"commercial-executions": {sql: `
		SELECT execution.execution_id,execution.version,execution.updated_at,
		       jsonb_build_object(
		           'initiative_id',execution.initiative_id,
		           'work_order_id',execution.work_order_id,
		           'work_order_hash',execution.work_order_hash,
		           'product_execution_id',execution.product_execution_id,
		           'customer_connection_id',execution.customer_connection_id,
		           'customer_connection_version',execution.customer_connection_version,
		           'financial_connection_id',execution.financial_connection_id,
		           'financial_connection_version',execution.financial_connection_version,
		           'gate_id',execution.gate_id,
		           'metric_id',execution.metric_id,
		           'metric_version',execution.metric_version,
		           'metric_hash',execution.metric_hash,
		           'state',execution.state,'current_phase',execution.current_phase,
		           'plan_hash',execution.plan_hash,
		           'controller_key_id',execution.controller_key_id,
		           'deadline',execution.deadline,
		           'created_at',execution.created_at,
		           'phases',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'phase',step.phase,'ordinal',step.ordinal,'state',step.state,
		               'attempt',step.attempt,'active_evidence_id',step.active_evidence_id,
		               'safe_code',step.safe_code,'updated_at',step.updated_at,
		               'completed_at',step.completed_at,
		               'evidence_hash',evidence.canonical_hash,
		               'evidence_disposition',evidence.disposition,
		               'issuer_key_id',evidence.issuer_key_id
		             ) ORDER BY step.ordinal)
		             FROM workforce_commercial_execution_steps step
		             LEFT JOIN workforce_commercial_execution_evidence evidence
		               ON evidence.tenant_id=step.tenant_id
		              AND evidence.organization_id=step.organization_id
		              AND evidence.execution_id=step.execution_id
		              AND evidence.evidence_id=step.active_evidence_id
		             WHERE step.tenant_id=execution.tenant_id
		               AND step.organization_id=execution.organization_id
		               AND step.execution_id=execution.execution_id
		           ),'[]'::jsonb),
		           'incidents',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'incident_id',incident.incident_id,'phase',incident.phase,
		               'kind',incident.kind,'safe_code',incident.safe_code,
		               'state',incident.state,'created_at',incident.created_at,
		               'resolved_at',incident.resolved_at
		             ) ORDER BY incident.created_at DESC,incident.incident_id)
		             FROM workforce_commercial_execution_incidents incident
		             WHERE incident.tenant_id=execution.tenant_id
		               AND incident.organization_id=execution.organization_id
		               AND incident.execution_id=execution.execution_id
		           ),'[]'::jsonb),
		           'corrections',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'correction_id',correction.correction_id,
		               'target_phase',correction.target_phase,
		               'target_evidence_id',correction.target_evidence_id,
		               'target_hash',correction.target_hash,'kind',correction.kind,
		               'reason',correction.reason,'status','applied',
		               'issued_at',correction.issued_at,'applied_at',correction.applied_at
		             ) ORDER BY correction.applied_at DESC,correction.correction_id)
		             FROM workforce_commercial_execution_corrections correction
		             WHERE correction.tenant_id=execution.tenant_id
		               AND correction.organization_id=execution.organization_id
		               AND correction.execution_id=execution.execution_id
		           ),'[]'::jsonb),
		           'recoveries',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'recovery_id',recovery.recovery_id,
		               'target_phase',recovery.target_phase,
		               'correction_id',recovery.correction_id,
		               'strategy',recovery.strategy,'state',recovery.state,
		               'issued_at',recovery.issued_at,'started_at',recovery.started_at,
		               'resolved_at',recovery.resolved_at
		             ) ORDER BY recovery.started_at DESC,recovery.recovery_id)
		             FROM workforce_commercial_execution_recoveries recovery
		             WHERE recovery.tenant_id=execution.tenant_id
		               AND recovery.organization_id=execution.organization_id
		               AND recovery.execution_id=execution.execution_id
		           ),'[]'::jsonb),
		           'measurement_proof',(
		             SELECT jsonb_build_object(
		               'evidence_id',evidence.evidence_id,
		               'evidence_hash',evidence.canonical_hash,
		               'disposition',evidence.disposition,
		               'subject_hash',evidence.subject_hash,
		               'issuer_key_id',evidence.issuer_key_id,
		               'reason_code',evidence.reason_code,
		               'observed_at',evidence.observed_at,
		               'captured_at',evidence.captured_at,
		               'sources',COALESCE((
		                 SELECT jsonb_agg(jsonb_build_object(
		                   'role',source.role,'source_kind',source.source_kind,
		                   'source_id_hash',source.source_id_hash,
		                   'source_version',source.source_version,
		                   'content_hash',source.content_hash,
		                   'operation',source.operation,'provider',source.provider,
		                   'account_ref_hash',source.account_ref_hash,
		                   'external_ref_hash',source.external_ref_hash,
		                   'related_id_hash',source.related_id_hash,
		                   'valuation_time',source.valuation_time,
		                   'source_state',source.source_state,
		                   'authority',source.authority,'observed_at',source.observed_at
		                 ) ORDER BY source.role,source.source_kind,source.source_id_hash)
		                 FROM workforce_commercial_execution_evidence_sources source
		                 WHERE source.tenant_id=evidence.tenant_id
		                   AND source.organization_id=evidence.organization_id
		                   AND source.evidence_id=evidence.evidence_id
		               ),'[]'::jsonb)
		             )
		             FROM workforce_commercial_execution_steps step
		             JOIN workforce_commercial_execution_evidence evidence
		               ON evidence.tenant_id=step.tenant_id
		              AND evidence.organization_id=step.organization_id
		              AND evidence.execution_id=step.execution_id
		              AND evidence.evidence_id=step.active_evidence_id
		             WHERE step.tenant_id=execution.tenant_id
		               AND step.organization_id=execution.organization_id
		               AND step.execution_id=execution.execution_id
		               AND step.phase='measurement'
		           ))
		FROM workforce_commercial_executions execution
		WHERE execution.tenant_id=$1 AND execution.organization_id=$2
		ORDER BY execution.updated_at DESC,execution.execution_id LIMIT $3 OFFSET $4`},
	"company-recovery": {sql: `
		SELECT recovery.id,recovery.version,recovery.updated_at,recovery.fields
		FROM (
		  SELECT policy.policy_id AS id,policy.version,head.updated_at,
		         jsonb_build_object(
		           'kind','limit_policy','scope_kind',policy.scope_kind,
		           'scope_id',policy.scope_id,'resource',policy.resource,
		           'soft_limit',policy.soft_limit::text,
		           'hard_limit',policy.hard_limit::text,
		           'window_micros',policy.window_micros::text,
		           'max_reservation_micros',policy.max_reservation_micros::text,
		           'open_circuit',policy.open_circuit,
		           'content_hash',policy.content_hash,
		           'effective_at',policy.effective_at,'expires_at',policy.expires_at,
		           'usage',COALESCE((
		             SELECT jsonb_build_object(
		               'window_started_at',usage.window_started_at,
		               'window_ends_at',usage.window_ends_at,
		               'used_units',usage.used_units::text,
		               'reserved_units',usage.reserved_units::text,
		               'updated_at',usage.updated_at)
		             FROM workforce_recovery_usage_windows usage
		             WHERE usage.tenant_id=policy.tenant_id
		               AND usage.organization_id=policy.organization_id
		               AND usage.policy_id=policy.policy_id
		               AND usage.policy_version=policy.version
		             ORDER BY usage.window_started_at DESC LIMIT 1
		           ),'{}'::jsonb),
		           'active_reservations',(
		             SELECT COUNT(*)::text
		             FROM workforce_recovery_reservation_bindings binding
		             JOIN workforce_recovery_reservations reservation
		               ON reservation.tenant_id=binding.tenant_id
		              AND reservation.organization_id=binding.organization_id
		              AND reservation.reservation_id=binding.reservation_id
		             WHERE binding.tenant_id=policy.tenant_id
		               AND binding.organization_id=policy.organization_id
		               AND binding.policy_id=policy.policy_id
		               AND binding.policy_version=policy.version
		               AND reservation.state='reserved'),
		           'circuit_state',COALESCE((
		             SELECT circuit.state FROM workforce_recovery_circuits circuit
		             WHERE circuit.tenant_id=policy.tenant_id
		               AND circuit.organization_id=policy.organization_id
		               AND circuit.scope_kind=policy.scope_kind
		               AND circuit.scope_id=policy.scope_id
		               AND circuit.resource=policy.resource
		           ),'closed')) AS fields
		  FROM workforce_recovery_limit_policy_heads head
		  JOIN workforce_recovery_limit_policies policy
		    ON policy.tenant_id=head.tenant_id
		   AND policy.organization_id=head.organization_id
		   AND policy.policy_id=head.policy_id AND policy.version=head.version
		  WHERE head.tenant_id=$1 AND head.organization_id=$2
		  UNION ALL
		  SELECT policy.policy_id,policy.version,head.updated_at,
		         jsonb_build_object(
		           'kind','recovery_policy',
		           'backup_interval_micros',policy.backup_interval_micros::text,
		           'rpo_micros',policy.rpo_micros::text,
		           'rto_micros',policy.rto_micros::text,
		           'pitr_required',policy.pitr_required,
		           'max_archive_bytes',policy.max_archive_bytes::text,
		           'content_hash',policy.content_hash,
		           'effective_at',policy.effective_at,'expires_at',policy.expires_at,
		           'retention_rules',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'data_class',rule.data_class,
		               'retention_micros',rule.retention_micros::text,
		               'action',rule.action) ORDER BY rule.data_class)
		             FROM workforce_recovery_retention_rules rule
		             WHERE rule.tenant_id=policy.tenant_id
		               AND rule.organization_id=policy.organization_id
		               AND rule.policy_id=policy.policy_id
		               AND rule.policy_version=policy.version
		           ),'[]'::jsonb))
		  FROM workforce_recovery_policy_heads head
		  JOIN workforce_recovery_policies policy
		    ON policy.tenant_id=head.tenant_id
		   AND policy.organization_id=head.organization_id
		   AND policy.policy_id=head.policy_id AND policy.version=head.version
		  WHERE head.tenant_id=$1 AND head.organization_id=$2
		  UNION ALL
		  SELECT backup.backup_id,1::BIGINT,backup.completed_at,
		         jsonb_build_object(
		           'kind','backup_manifest','scope_kind',backup.scope_kind,
		           'scope_id',backup.scope_id,'state',backup.state,
		           'authorization_hash',backup.authorization_hash,
		           'manifest_hash',backup.manifest_hash,
		           'archive_hash',backup.archive_hash,
		           'archive_available',backup.encrypted_archive IS NOT NULL,
		           'key_erased',backup.key_erased,
		           'pitr_available',backup.pitr_point IS NOT NULL,
		           'snapshot_at',backup.snapshot_at,'completed_at',backup.completed_at,
		           'erased_at',backup.erased_at)
		  FROM workforce_recovery_backups backup
		  WHERE backup.tenant_id=$1 AND backup.organization_id=$2
		  UNION ALL
		  SELECT restore.restore_id,1::BIGINT,restore.created_at,
		         jsonb_build_object(
		           'kind','restore','backup_id',restore.backup_id,
		           'archive_hash',restore.archive_hash,'mode',restore.mode,
		           'target_at',restore.target_at,'state',restore.state,
		           'receipt_hash',restore.receipt_hash,
		           'restored_tables',restore.restored_tables::text,
		           'restored_rows',restore.restored_rows::text,
		           'cancelled_runtime_leases',restore.cancelled_runtime_leases::text,
		           'invalidated_authority_leases',restore.invalidated_authority_leases::text,
		           'coalesced_wakes',restore.coalesced_wakes::text,
		           'quarantined_effects',restore.quarantined_effects::text,
		           'quarantined_external_state',restore.quarantined_external_state::text,
		           'rto_micros',restore.rto_micros::text,'rto_status',restore.rto_status,
		           'reconciliation_evidence_hash',restore.reconciliation_evidence_hash,
		           'started_at',restore.started_at,'completed_at',restore.completed_at,
		           'reconciled_at',restore.reconciled_at)
		  FROM workforce_recovery_restores restore
		  WHERE restore.tenant_id=$1 AND restore.organization_id=$2
		  UNION ALL
		  SELECT erasure.erasure_id,1::BIGINT,erasure.executed_at,
		         jsonb_build_object(
		           'kind','erasure_receipt','source','directive',
		           'target_kind',erasure.target_kind,'target_id',erasure.target_id,
		           'data_class',erasure.data_class,'action',erasure.action,
		           'receipt_hash',erasure.receipt_hash,
		           'destroyed_keys',erasure.destroyed_keys::text,
		           'deleted_objects',erasure.deleted_objects::text,
		           'executed_at',erasure.executed_at)
		  FROM workforce_recovery_erasures erasure
		  WHERE erasure.tenant_id=$1 AND erasure.organization_id=$2
		  UNION ALL
		  SELECT retention.execution_id,retention.policy_version,retention.executed_at,
		         jsonb_build_object(
		           'kind','erasure_receipt','source','retention',
		           'policy_id',retention.policy_id,
		           'policy_version',retention.policy_version,
		           'target_kind','backup','target_id',retention.backup_id,
		           'action',retention.action,'receipt_hash',retention.receipt_hash,
		           'destroyed_keys',retention.destroyed_keys::text,
		           'deleted_objects',retention.deleted_objects::text,
		           'executed_at',retention.executed_at)
		  FROM workforce_recovery_retention_executions retention
		  WHERE retention.tenant_id=$1 AND retention.organization_id=$2
		  UNION ALL
		  SELECT batch.batch_id,batch.sequence,batch.reconciled_at,
		         jsonb_build_object(
		           'kind','offline_batch','machine_id',batch.machine_id,
		           'sequence',batch.sequence::text,'base_backup_id',batch.base_backup_id,
		           'base_archive_hash',batch.base_archive_hash,
		           'batch_hash',batch.batch_hash,'receipt_hash',batch.receipt_hash,
		           'contiguous',batch.contiguous,
		           'created_at',batch.created_at,'reconciled_at',batch.reconciled_at,
		           'dispositions',COALESCE((
		             SELECT jsonb_object_agg(summary.disposition,summary.total)
		             FROM (
		               SELECT record.disposition,COUNT(*)::text AS total
		               FROM workforce_recovery_offline_records record
		               WHERE record.tenant_id=batch.tenant_id
		                 AND record.organization_id=batch.organization_id
		                 AND record.batch_id=batch.batch_id
		               GROUP BY record.disposition
		             ) summary
		           ),'{}'::jsonb),
		           'conflicts',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'reconciliation_id',conflict.reconciliation_id,
		               'record_kind',conflict.record_kind,'record_id',conflict.record_id,
		               'version',conflict.version,'offline_hash',conflict.offline_hash,
		               'current_hash',conflict.current_hash,'state',conflict.state,
		               'created_at',conflict.created_at,'resolved_at',conflict.resolved_at
		             ) ORDER BY conflict.created_at,conflict.reconciliation_id)
		             FROM workforce_recovery_offline_reconciliation conflict
		             WHERE conflict.tenant_id=batch.tenant_id
		               AND conflict.organization_id=batch.organization_id
		               AND conflict.batch_id=batch.batch_id
		           ),'[]'::jsonb))
		  FROM workforce_recovery_offline_batches batch
		  WHERE batch.tenant_id=$1 AND batch.organization_id=$2
		  UNION ALL
		  SELECT incident.incident_id,1::BIGINT,incident.created_at,
		         jsonb_build_object(
		           'kind','recovery_incident','incident_kind',incident.incident_kind,
		           'scope_kind',incident.scope_kind,'scope_id',incident.scope_id,
		           'resource',incident.resource,'safe_code',incident.safe_code,
		           'record_kind',incident.record_kind,'record_id',incident.record_id,
		           'observed_value',incident.observed_value::text,
		           'limit_value',incident.limit_value::text,
		           'canonical_hash',incident.canonical_hash,
		           'created_at',incident.created_at)
		  FROM workforce_recovery_incidents incident
		  WHERE incident.tenant_id=$1 AND incident.organization_id=$2
		  UNION ALL
		  SELECT circuit.scope_kind||':'||circuit.scope_id||':'||circuit.resource,
		         circuit.version,circuit.updated_at,
		         jsonb_build_object(
		           'kind','recovery_circuit','scope_kind',circuit.scope_kind,
		           'scope_id',circuit.scope_id,'resource',circuit.resource,
		           'state',circuit.state,'reason_code',circuit.reason_code,
		           'opened_at',circuit.opened_at,'closed_at',circuit.closed_at)
		  FROM workforce_recovery_circuits circuit
		  WHERE circuit.tenant_id=$1 AND circuit.organization_id=$2
		  UNION ALL
		  SELECT shutdown.shutdown_id,1::BIGINT,head.updated_at,
		         jsonb_build_object(
		           'kind','shutdown','state',shutdown.state,
		           'reason_code',shutdown.reason_code,
		           'canonical_hash',shutdown.canonical_hash,
		           'released_reservations',shutdown.released_reservations::text,
		           'cancelled_leases',shutdown.cancelled_leases::text,
		           'coalesced_wakes',shutdown.coalesced_wakes::text,
		           'quarantined_effects',shutdown.quarantined_effects::text,
		           'started_at',shutdown.started_at,'completed_at',shutdown.completed_at)
		  FROM workforce_recovery_shutdown_heads head
		  JOIN workforce_recovery_shutdowns shutdown
		    ON shutdown.tenant_id=head.tenant_id
		   AND shutdown.organization_id=head.organization_id
		   AND shutdown.shutdown_id=head.shutdown_id
		  WHERE head.tenant_id=$1 AND head.organization_id=$2
		) recovery
		ORDER BY recovery.updated_at DESC,recovery.id LIMIT $3 OFFSET $4`},
	"autonomous-release": {sql: `
		SELECT release.id,release.version,release.updated_at,release.fields
		FROM (
		  SELECT property.property_id AS id,property.version,head.updated_at,
		         jsonb_build_object(
		           'kind','property','property_kind',property.property_kind,
		           'initiative_id',property.initiative_id,'state',property.state,
		           'why',jsonb_build_object(
		             'source','persisted_property_evidence',
		             'evidence_count',(
		               SELECT COUNT(*) FROM workforce_autonomous_company_property_evidence evidence
		               WHERE evidence.tenant_id=property.tenant_id
		                 AND evidence.organization_id=property.organization_id
		                 AND evidence.property_id=property.property_id
		                 AND evidence.property_version=property.version),
		             'process_count',(
		               SELECT COUNT(*) FROM workforce_autonomous_company_property_processes process
		               WHERE process.tenant_id=property.tenant_id
		                 AND process.organization_id=property.organization_id
		                 AND process.property_id=property.property_id
		                 AND process.property_version=property.version),
		             'lineage_count',(
		               SELECT COUNT(*) FROM workforce_autonomous_company_property_lineage lineage
		               WHERE lineage.tenant_id=property.tenant_id
		                 AND lineage.organization_id=property.organization_id
		                 AND lineage.property_id=property.property_id
		                 AND lineage.property_version=property.version)),
		           'action',NULL,
		           'canonical_hash',property.canonical_hash,
		           'started_at',property.started_at,'evaluated_at',property.evaluated_at,
		           'fresh_until',property.fresh_until,'completed_at',property.completed_at,
		           'evidence_summary',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'evidence_kind',evidence.evidence_kind,
		               'initiative_id',evidence.initiative_id,
		               'record_id',evidence.record_id,
		               'record_version',evidence.record_version,
		               'record_hash',evidence.record_hash,
		               'source_state',evidence.source_state,
		               'validity',evidence.validity,
		               'reconciliation',evidence.reconciliation,
		               'contaminated',evidence.contaminated,
		               'observed_at',evidence.observed_at,
		               'fresh_until',evidence.fresh_until
		             ) ORDER BY evidence.evidence_kind,evidence.record_id,evidence.record_version)
		             FROM workforce_autonomous_company_property_evidence evidence
		             WHERE evidence.tenant_id=property.tenant_id
		               AND evidence.organization_id=property.organization_id
		               AND evidence.property_id=property.property_id
		               AND evidence.property_version=property.version
		           ),'[]'::jsonb),
		           'process_summary',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'process_id',process.process_id,'wake_id',process.wake_id,
		               'seat_id',process.seat_id,'department_id',process.department_id,
		               'role',process.role,'memoryless',process.memoryless,
		               'fresh_process',process.fresh_process,
		               'evidence_id',process.evidence_id,
		               'evidence_hash',process.evidence_hash,
		               'started_at',process.started_at,'observed_at',process.observed_at
		             ) ORDER BY process.role,process.process_id)
		             FROM workforce_autonomous_company_property_processes process
		             WHERE process.tenant_id=property.tenant_id
		               AND process.organization_id=property.organization_id
		               AND process.property_id=property.property_id
		               AND process.property_version=property.version
		           ),'[]'::jsonb),
		           'lineage_summary',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'position',lineage.position,'stage',lineage.stage,
		               'record_id',lineage.record_id,
		               'record_version',lineage.record_version,
		               'record_hash',lineage.record_hash,
		               'observed_at',lineage.observed_at
		             ) ORDER BY lineage.position)
		             FROM workforce_autonomous_company_property_lineage lineage
		             WHERE lineage.tenant_id=property.tenant_id
		               AND lineage.organization_id=property.organization_id
		               AND lineage.property_id=property.property_id
		               AND lineage.property_version=property.version
		           ),'[]'::jsonb)) AS fields
		  FROM workforce_autonomous_company_property_heads head
		  JOIN workforce_autonomous_company_property_records property
		    ON property.tenant_id=head.tenant_id
		   AND property.organization_id=head.organization_id
		   AND property.property_id=head.property_id
		   AND property.version=head.version
		  WHERE head.tenant_id=$1 AND head.organization_id=$2
		  UNION ALL
		  SELECT plan.plan_id,GREATEST(head.last_event_sequence,1::BIGINT),head.updated_at,
		         jsonb_build_object(
		           'kind','next_cycle','initiative_id',plan.initiative_id,
		           'state',head.state,'action',plan.selected_action,
		           'why',jsonb_build_object(
		             'source','persisted_learning_conclusion',
		             'conclusion_id',plan.conclusion_id,
		             'conclusion_hash',plan.conclusion_hash,
		             'hypothesis_id',plan.hypothesis_id,
		             'portfolio_feedback_id',plan.portfolio_feedback_id),
		           'canonical_hash',plan.canonical_hash,
		           'due_at',plan.due_at,'claimed_at',plan.claimed_at,
		           'last_event_sequence',head.last_event_sequence,
		           'operations',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'sequence',operation.sequence,
		               'operation_kind',operation.operation_kind,
		               'action',COALESCE(operation.decision,operation.runtime_action),
		               'from_state',operation.from_state,'to_state',operation.to_state,
		               'decision',operation.decision,'runtime_action',operation.runtime_action
		             ) ORDER BY operation.sequence)
		             FROM workforce_autonomous_company_next_cycle_operations operation
		             WHERE operation.tenant_id=plan.tenant_id
		               AND operation.organization_id=plan.organization_id
		               AND operation.plan_id=plan.plan_id
		           ),'[]'::jsonb),
		           'events',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'event_id',event.event_id,'sequence',event.sequence,
		               'state',event.state,'canonical_hash',event.canonical_hash,
		               'occurred_at',event.occurred_at,
		               'why',jsonb_build_object(
		                 'source','persisted_event_evidence',
		                 'evidence_count',(
		                   SELECT COUNT(*)
		                   FROM workforce_autonomous_company_next_cycle_event_evidence evidence
		                   WHERE evidence.tenant_id=event.tenant_id
		                     AND evidence.organization_id=event.organization_id
		                     AND evidence.event_id=event.event_id)),
		               'evidence_summary',COALESCE((
		                 SELECT jsonb_agg(jsonb_build_object(
		                   'evidence_kind',evidence.evidence_kind,
		                   'initiative_id',evidence.initiative_id,
		                   'record_id',evidence.record_id,
		                   'record_version',evidence.record_version,
		                   'record_hash',evidence.record_hash,
		                   'source_state',evidence.source_state,
		                   'validity',evidence.validity,
		                   'reconciliation',evidence.reconciliation,
		                   'contaminated',evidence.contaminated,
		                   'observed_at',evidence.observed_at,
		                   'fresh_until',evidence.fresh_until
		                 ) ORDER BY evidence.evidence_kind,evidence.record_id,evidence.record_version)
		                 FROM workforce_autonomous_company_next_cycle_event_evidence evidence
		                 WHERE evidence.tenant_id=event.tenant_id
		                   AND evidence.organization_id=event.organization_id
		                   AND evidence.event_id=event.event_id
		               ),'[]'::jsonb)
		             ) ORDER BY event.sequence)
		             FROM workforce_autonomous_company_next_cycle_events event
		             WHERE event.tenant_id=plan.tenant_id
		               AND event.organization_id=plan.organization_id
		               AND event.plan_id=plan.plan_id
		           ),'[]'::jsonb))
		  FROM workforce_autonomous_company_next_cycle_heads head
		  JOIN workforce_autonomous_company_next_cycle_plans plan
		    ON plan.tenant_id=head.tenant_id
		   AND plan.organization_id=head.organization_id
		   AND plan.plan_id=head.plan_id
		  WHERE head.tenant_id=$1 AND head.organization_id=$2
		) release
		ORDER BY release.updated_at DESC,release.id LIMIT $3 OFFSET $4`},
	"experiments": {sql: `
		SELECT head.record_id,head.latest_version,head.updated_at,
		       jsonb_build_object(
		           'kind',head.kind,'domain',head.domain,'state',head.state,
		           'initiative_id',record.initiative_id,
		           'observation_kind',record.observation_kind,
		           'truth_status',record.truth_status,
		           'observed_at',record.observed_at,
		           'effective_at',record.effective_at,
		           'expires_at',record.expires_at,
		           'content_hash',record.content_hash)
		FROM workforce_company_state_heads head
		JOIN workforce_company_state_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.record_id=head.record_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND lower(head.kind) IN ('hypothesis','experiment')
		ORDER BY head.updated_at DESC,head.record_id LIMIT $3 OFFSET $4`},
	"learning": {sql: `
		SELECT head.hypothesis_id,1,head.updated_at,
		       jsonb_build_object(
		           'initiative_id',head.initiative_id,'state',head.state,
		           'inconclusive_count',head.inconclusive_count,
		           'review_at',head.review_at,
		           'maximum_duration_at',head.maximum_duration_at,
		           'conclusion_id',head.conclusion_id,
		           'observation_count',(
		             SELECT COUNT(*) FROM workforce_learning_observation_index observation
		             WHERE observation.tenant_id=head.tenant_id
		               AND observation.organization_id=head.organization_id
		               AND observation.hypothesis_id=head.hypothesis_id
		           ))
		FROM workforce_learning_hypothesis_heads head
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.hypothesis_id LIMIT $3 OFFSET $4`},
	"learning-next-actions": {sql: `
		SELECT conclusion_id,1,created_at,
		       jsonb_build_object(
		           'hypothesis_id',hypothesis_id,'initiative_id',initiative_id,
		           'next_action',next_action,
		           'portfolio_feedback_id',portfolio_feedback_id,
		           'due_at',due_at,'state',state,
		           'claimed_at',claimed_at,'dispatched_at',dispatched_at,
		           'completed_at',completed_at)
		FROM workforce_learning_next_cycles
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,conclusion_id LIMIT $3 OFFSET $4`},
	"ambiguities": {sql: `
		SELECT 'effect:'||proposal_id,1,updated_at,
		       jsonb_build_object(
		           'source','external_effect','source_id',proposal_id,
		           'operation',operation,'state',state,
		           'safe_code',safe_error_code,'external_id',external_id)
		FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2
		  AND state='externally_ambiguous'
		ORDER BY updated_at DESC,proposal_id LIMIT $3 OFFSET $4`},
	"founder-actions": {sql: `
		SELECT command.command_id,command.resulting_version,command.created_at,
		       jsonb_build_object(
		           'owner_id',command.owner_id,'action',command.action,
		           'resource_kind',command.resource_kind,'resource_id',command.resource_id,
		           'expected_version',command.expected_version,
		           'resulting_version',command.resulting_version,
		           'change_hash',command.change_hash,'change',command.change,
		           'key_id',command.key_id,'effective_at',command.effective_at,
		           'state',COALESCE(event.fields->>'state','pending'))
		FROM workforce_owner_commands command
		LEFT JOIN workforce_lifecycle_events event
		  ON event.tenant_id=command.tenant_id
		 AND event.organization_id=command.organization_id
		 AND event.event_id='event:'||command.command_id
		WHERE command.tenant_id=$1 AND command.organization_id=$2
		ORDER BY command.created_at DESC,command.command_id LIMIT $3 OFFSET $4`},
	"authority-change-receipts": {sql: `
		SELECT receipt_id,authority_version,created_at,
		       jsonb_build_object(
		           'authority_kind',authority_kind,'authority_id',authority_id,
		           'affected_authority_lease_count',affected_authority_lease_count,
		           'affected_runtime_lease_count',affected_runtime_lease_count,
		           'affected_queued_wake_count',affected_queued_wake_count,
		           'affected_dispatched_wake_count',affected_dispatched_wake_count,
		           'affected_effect_count',affected_effect_count)
		FROM workforce_company_authority_change_receipts
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY created_at DESC,receipt_id LIMIT $3 OFFSET $4`},
	"security-reviews": {sql: `
		SELECT head.threat_model_id,head.version,head.updated_at,
		       jsonb_build_object(
		           'state',head.state,'threat_model_hash',head.threat_model_hash,
		           'qualification_id',head.qualification_id,
		           'expires_at',head.expires_at,
		           'coverage',COALESCE((
		             SELECT jsonb_agg(jsonb_build_object(
		               'boundary',coverage.boundary,'review_id',coverage.review_id,
		               'outcome',coverage.outcome,
		               'reviewer_seat_id',coverage.reviewer_seat_id,
		               'reviewer_department_id',coverage.reviewer_department_id,
		               'reviewed_at',coverage.reviewed_at
		             ) ORDER BY coverage.boundary)
		             FROM workforce_security_review_coverage coverage
		             WHERE coverage.tenant_id=head.tenant_id
		               AND coverage.organization_id=head.organization_id
		               AND coverage.threat_model_id=head.threat_model_id
		               AND coverage.threat_model_version=head.version
		           ),'[]'::jsonb))
		FROM workforce_security_qualification_heads head
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,head.qualification_id LIMIT $3 OFFSET $4`},
	"founder-projections": {sql: `
		SELECT receipt.receipt_id,receipt.version,head.updated_at,
		       jsonb_build_object(
		           'initiative_id',receipt.initiative_id,
		           'snapshot_hash',receipt.server_snapshot_hash,
		           'snapshot_cursor',receipt.snapshot_cursor,
		           'resource_counts',receipt.resource_counts,
		           'resource_versions',receipt.resource_versions,
		           'owner_id',receipt.owner_id,'process_id',receipt.process_id,
		           'wake_id',receipt.wake_id,'process_runtime',receipt.process_runtime,
		           'process_role',receipt.process_role,'fresh_process',receipt.fresh_process,
		           'render_evidence_id',receipt.render_evidence_id,
		           'render_evidence_hash',receipt.render_evidence_hash,
		           'rendered_at',receipt.rendered_at,'expires_at',receipt.expires_at,
		           'canonical_hash',receipt.canonical_hash,
		           'signer_key_id',receipt.signer_key_id,
		           'fresh',receipt.expires_at>CURRENT_TIMESTAMP)
		FROM workforce_founder_projection_receipt_heads head
		JOIN workforce_founder_projection_receipts receipt
		  ON receipt.tenant_id=head.tenant_id
		 AND receipt.organization_id=head.organization_id
		 AND receipt.receipt_id=head.receipt_id AND receipt.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.updated_at DESC,receipt.receipt_id LIMIT $3 OFFSET $4`},
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
