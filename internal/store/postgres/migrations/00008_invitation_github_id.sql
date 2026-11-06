-- +goose Up
-- A GitHub-login invitation is for the account that had the login when it was
-- created: a login can be renamed and then taken by someone else.
ALTER TABLE invitations ADD COLUMN github_user_id bigint CHECK (github_user_id > 0);

-- +goose Down
ALTER TABLE invitations DROP COLUMN github_user_id;
