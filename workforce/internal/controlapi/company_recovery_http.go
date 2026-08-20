package controlapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"centra/workforce/internal/companyrecovery"
)

func (service *Service) handleCompanyLimitPolicy(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.LimitPolicy](writer, request, "company limit policy")
	if !ok {
		return
	}
	record, replayed, err := service.RegisterCompanyLimitPolicy(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"policy": record, "replayed": replayed})
}

func (service *Service) handleCompanyRecoveryPolicy(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.RecoveryPolicy](writer, request, "company recovery policy")
	if !ok {
		return
	}
	record, replayed, err := service.RegisterCompanyRecoveryPolicy(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"policy": record, "replayed": replayed})
}

func (service *Service) handleCompanyMachineKey(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.MachineKeyRegistration](writer, request, "company machine key")
	if !ok {
		return
	}
	record, replayed, err := service.RegisterCompanyMachineKey(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"machine_key": record, "replayed": replayed})
}

func (service *Service) handleCompanyUsageAdmission(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.UsageRequest](writer, request, "company usage request")
	if !ok {
		return
	}
	receipt, err := service.AdmitCompanyUsage(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, receipt)
}

func (service *Service) handleCompanyUsageCommit(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[UsageCommitRequest](writer, request, "company usage commit")
	if !ok {
		return
	}
	receipt, err := service.CommitCompanyUsage(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, receipt)
}

func (service *Service) handleCompanyUsageRelease(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[UsageReleaseRequest](writer, request, "company usage release")
	if !ok {
		return
	}
	receipt, err := service.ReleaseCompanyUsage(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, receipt)
}

func (service *Service) handleCompanyMetric(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.MetricSample](writer, request, "company metric")
	if !ok {
		return
	}
	record, replayed, err := service.RecordCompanyMetric(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"metric": record, "replayed": replayed})
}

func (service *Service) handleCompanyTrace(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.TraceSpan](writer, request, "company trace")
	if !ok {
		return
	}
	record, replayed, err := service.RecordCompanyTrace(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"trace": record, "replayed": replayed})
}

func (service *Service) handleCompanyIncident(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.Incident](writer, request, "company incident")
	if !ok {
		return
	}
	record, replayed, err := service.RecordCompanyIncident(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"incident": record, "replayed": replayed})
}

func (service *Service) handleCompanyShutdownBegin(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[ShutdownRequest](writer, request, "company shutdown request")
	if !ok {
		return
	}
	receipt, err := service.BeginCompanyShutdown(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, receipt)
}

func (service *Service) handleCompanyShutdownComplete(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[ShutdownCompletionRequest](writer, request, "company shutdown completion")
	if !ok {
		return
	}
	receipt, err := service.CompleteCompanyShutdown(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, receipt)
}

func (service *Service) handleCompanyBackupCreate(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.BackupAuthorization](writer, request, "company backup authorization")
	if !ok {
		return
	}
	bundle, replayed, err := service.CreateCompanyBackup(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"bundle": bundle, "replayed": replayed})
}

func (service *Service) handleCompanyBackup(writer http.ResponseWriter, request *http.Request) {
	id := companyrecovery.BackupID(strings.TrimSpace(request.PathValue("backup_id")))
	bundle, err := service.CompanyBackup(request.Context(), principalFrom(request), id)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, bundle)
}

func (service *Service) handleCompanyBackupImport(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[BackupImportRequest](writer, request, "company backup import")
	if !ok {
		return
	}
	replayed, err := service.ImportCompanyBackup(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"replayed": replayed})
}

func (service *Service) handleCompanyRestore(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.RestoreAuthorization](writer, request, "company restore authorization")
	if !ok {
		return
	}
	receipt, replayed, err := service.RestoreCompanyBackup(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"receipt": receipt, "replayed": replayed})
}

func (service *Service) handleCompanyRestoreAcknowledgement(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[RestoreAcknowledgementRequest](writer, request, "company restore acknowledgement")
	if !ok {
		return
	}
	receipt, err := service.AcknowledgeCompanyRestore(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, receipt)
}

func (service *Service) handleCompanyErasure(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.ErasureDirective](writer, request, "company erasure directive")
	if !ok {
		return
	}
	receipt, replayed, err := service.ExecuteCompanyErasure(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"receipt": receipt, "replayed": replayed})
}

func (service *Service) handleCompanyRetention(writer http.ResponseWriter, request *http.Request) {
	receipts, err := service.ApplyCompanyRetention(request.Context(), principalFrom(request))
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, receipts)
}

func (service *Service) handleCompanyOfflineBatch(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.OfflineBatch](writer, request, "company offline batch")
	if !ok {
		return
	}
	receipt, replayed, err := service.CoalesceCompanyOfflineBatch(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"receipt": receipt, "replayed": replayed})
}

func (service *Service) handleCompanyOfflineResolution(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.OfflineReconciliationResolution](writer, request, "company offline reconciliation resolution")
	if !ok {
		return
	}
	replayed, err := service.ResolveCompanyOfflineReconciliation(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"replayed": replayed})
}

func (service *Service) handleCompanyRecoveryQualification(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[companyrecovery.RecoveryQualification](writer, request, "company recovery qualification")
	if !ok {
		return
	}
	replayed, err := service.CommitCompanyRecoveryQualification(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, mutationStatus(replayed), map[string]any{"replayed": replayed})
}

func (service *Service) handleCompanyCircuitOpen(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[RecoveryCircuitRequest](writer, request, "company circuit request")
	if !ok {
		return
	}
	if err := service.OpenCompanyCircuit(request.Context(), principalFrom(request), value); err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"state": "open"})
}

func (service *Service) handleCompanyCircuitClose(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[RecoveryCircuitRequest](writer, request, "company circuit request")
	if !ok {
		return
	}
	if err := service.CloseCompanyCircuit(request.Context(), principalFrom(request), value); err != nil {
		writeCompanyRecoveryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"state": "closed"})
}

func decodeCompanyRecoveryRequest[T any](
	writer http.ResponseWriter,
	request *http.Request,
	label string,
) (T, bool) {
	var value T
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid "+label)
		return value, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid "+label)
		return value, false
	}
	return value, true
}

func mutationStatus(replayed bool) int {
	if replayed {
		return http.StatusOK
	}
	return http.StatusCreated
}

func writeCompanyRecoveryError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, companyrecovery.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, ErrConflict), errors.Is(err, companyrecovery.ErrConflict),
		errors.Is(err, companyrecovery.ErrNotFound), errors.Is(err, companyrecovery.ErrLimitExceeded),
		errors.Is(err, companyrecovery.ErrNoLimitPolicy), errors.Is(err, companyrecovery.ErrCircuitOpen),
		errors.Is(err, companyrecovery.ErrDraining), errors.Is(err, companyrecovery.ErrRestoreQuarantined),
		errors.Is(err, companyrecovery.ErrRestoreTargetNotClean),
		errors.Is(err, companyrecovery.ErrArchiveErased), errors.Is(err, companyrecovery.ErrOfflineFork),
		errors.Is(err, companyrecovery.ErrReconciliationRequired):
		status = http.StatusConflict
	}
	writeError(writer, status, err.Error())
}
