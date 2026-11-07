-- +goose Up
-- Whether SCIM suspended the membership (identity.md, SCIM): SCIM lets back
-- in only whom it suspended, never someone an Owner suspended by hand.
-- Every other change of status clears it.
ALTER TABLE memberships ADD COLUMN scim_suspended boolean NOT NULL DEFAULT false;
ALTER TABLE memberships ADD CONSTRAINT memberships_scim_suspended CHECK (NOT scim_suspended OR status = 'suspended');

-- +goose Down
ALTER TABLE memberships DROP COLUMN scim_suspended;
