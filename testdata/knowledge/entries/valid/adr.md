---
title: Queue jobs in Postgres
author_role: architect
source: JOB-41
created: 2026-10-01
last_verified: 2026-10-20T09:30:00Z
---
# ADR 1: Queue jobs in Postgres

We lease jobs with SELECT ... FOR UPDATE SKIP LOCKED.
