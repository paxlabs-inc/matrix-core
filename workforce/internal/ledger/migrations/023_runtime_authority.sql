ALTER TABLE workforce_authority_records
    DROP CONSTRAINT workforce_authority_records_authority_kind_check;

ALTER TABLE workforce_authority_records
    ADD CONSTRAINT workforce_authority_records_authority_kind_check CHECK (
        authority_kind IN (
            'organization', 'mandate', 'seat', 'policy', 'runtime_authority'
        )
    );
