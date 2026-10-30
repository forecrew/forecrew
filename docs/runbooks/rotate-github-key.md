# Rotate the GitHub App private key

forecrewd reads the key at startup, so a rotation needs a restart. Workers are
not affected: they get a fresh repo token from forecrewd for every job and
never see the key.

1. In GitHub, open the App's settings, **Private keys**, and generate a new key.
   Keep the old one until step 4; an App can hold several keys.
2. Put the new PEM in `.env` as `FORECREW_GITHUB_PRIVATE_KEY` (one line, double
   quotes, `\n` for line breaks):

   ```sh
   awk 'NF{printf "%s\\n", $0}' new-key.pem
   ```

   With systemd, edit `/etc/forecrew/forecrewd.env` instead.
3. Restart and check:

   ```sh
   docker compose up -d forecrewd         # systemd: sudo systemctl restart forecrewd
   docker compose logs --tail 20 forecrewd
   curl -fsS https://<host>/healthz
   ```

   A key that cannot be read stops forecrewd at startup with a message in the
   log. To prove the new key works, queue any job (or redeliver a webhook) and see it
   leased: minting its repo token uses the key.
4. Delete the old key in GitHub.
5. Remove the new PEM file from your machine's disk and shell history.

If the old key leaked, do step 4 first: webhooks keep working, jobs fail to get
repo tokens until step 3 is done.

## Webhook secret

Same procedure: change the secret in the App's settings and
`FORECREW_GITHUB_WEBHOOK_SECRET` in `.env`, restart forecrewd. Deliveries
sent between the two changes fail signature checks; redeliver them from the
App's **Advanced** tab.
