CREATE TABLE IF NOT EXISTS agents (
  agent_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  capabilities TEXT NOT NULL DEFAULT '[]',
  tools TEXT NOT NULL DEFAULT '[]',
  skills TEXT NOT NULL DEFAULT '[]',
  constraints TEXT NOT NULL DEFAULT '{}',
  preferred_roles TEXT NOT NULL DEFAULT '[]',
  evidence TEXT NOT NULL DEFAULT '[]',
  operator TEXT,
  load REAL NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'CREATED',
  metadata TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS credentials (
  credential_id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agents(agent_id),
  token_hash TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'active',
  rotated_from TEXT,
  issued_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_credentials_agent ON credentials(agent_id);

CREATE TABLE IF NOT EXISTS credential_audit (
  event_id TEXT PRIMARY KEY,
  actor TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  credential_id TEXT NOT NULL,
  action TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TRIGGER IF NOT EXISTS credential_audit_no_update BEFORE UPDATE ON credential_audit BEGIN SELECT RAISE(ABORT,'audit is append-only'); END;
CREATE TRIGGER IF NOT EXISTS credential_audit_no_delete BEFORE DELETE ON credential_audit BEGIN SELECT RAISE(ABORT,'audit is append-only'); END;
CREATE TABLE IF NOT EXISTS credential_operator_updates (
  update_id INTEGER PRIMARY KEY,
  agent_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
  task_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  objective TEXT NOT NULL,
  context TEXT NOT NULL DEFAULT '{}',
  constraints TEXT NOT NULL DEFAULT '[]',
  required_capabilities TEXT NOT NULL DEFAULT '[]',
  required_tools TEXT NOT NULL DEFAULT '[]',
  success_criteria TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'OPEN',
  owner TEXT REFERENCES agents(agent_id),
  team_id TEXT,
  project_id TEXT,
  created_by TEXT NOT NULL REFERENCES agents(agent_id),
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  deadline TEXT,
  claimed_at TEXT,
  lease_expires_at TEXT,
  claim_id TEXT,
  submitted_message_id TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_idem ON tasks(created_by, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS messages (
  -- seq is the inbox cursor key: a monotonic insertion sequence. Paginating
  -- on received_at instead would be wrong — several messages routinely land
  -- in the same second, and any tie-break on message_id (a random uuid)
  -- orders them arbitrarily, letting a cursor skip a message permanently.
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id TEXT NOT NULL UNIQUE,
  protocol_version TEXT NOT NULL,
  type TEXT NOT NULL,
  sender TEXT NOT NULL,
  recipient TEXT NOT NULL,
  ts TEXT NOT NULL,
  conversation_id TEXT,
  task_id TEXT,
  priority TEXT NOT NULL DEFAULT 'normal',
  payload TEXT NOT NULL,
  evidence TEXT NOT NULL DEFAULT '[]',
  reply_to TEXT,
  ttl INTEGER,
  received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_inbox ON messages(recipient, seq);
CREATE INDEX IF NOT EXISTS idx_messages_task ON messages(task_id);

-- Projects (RFC-1250). `open` groups records that stay visible to everyone
-- (§11); `closed` restricts them to members (§5-§8). A project always has
-- exactly one owner (§9): ownership moves only by an accepted transfer, or by
-- succession when the owner departs.
CREATE TABLE IF NOT EXISTS projects (
  project_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  name TEXT NOT NULL,
  objective TEXT NOT NULL DEFAULT '',
  visibility TEXT NOT NULL DEFAULT 'open',
  listed INTEGER NOT NULL DEFAULT 1,
  membership_policy TEXT NOT NULL DEFAULT 'invite_only',
  owner TEXT NOT NULL REFERENCES agents(agent_id),
  status TEXT NOT NULL DEFAULT 'active',
  -- A transfer of ownership is bilateral: the current owner nominates, the
  -- nominee accepts (RFC-1250 §9, and the counterparty-as-approver case in
  -- RFC-1600 §8). Only one nomination can be outstanding — a project has
  -- exactly one owner, so a queue of them would be meaningless.
  pending_owner TEXT REFERENCES agents(agent_id),
  pending_owner_at TEXT,
  created_at TEXT NOT NULL,
  metadata TEXT NOT NULL DEFAULT '{}'
);

-- Project membership (RFC-1250 §7). Rows exist only for closed projects:
-- open ones are visible to everyone, so there is nothing to record.
--
-- invited_by distinguishes the two pending shapes (RFC-1250 §7): non-null
-- means the project invited the agent and the agent must accept; null means
-- the agent applied and the owner must approve.
CREATE TABLE IF NOT EXISTS project_members (
  project_id TEXT NOT NULL REFERENCES projects(project_id),
  agent_id TEXT NOT NULL REFERENCES agents(agent_id),
  status TEXT NOT NULL,            -- invited|active|revoked|left|reviewer
  -- role is orthogonal to status: a coordinator is an active member with the
  -- authority to invite and remove others (RFC-1250 §2).
  role TEXT NOT NULL DEFAULT 'member',  -- member|coordinator
  invited_by TEXT,
  joined_at TEXT,
  scope TEXT,                      -- reviewer only: the single task_id in view
  expires_at TEXT,                 -- reviewer only: mandatory time box
  created_at TEXT NOT NULL,
  PRIMARY KEY (project_id, agent_id)
);
CREATE INDEX IF NOT EXISTS idx_members_agent ON project_members(agent_id, status);

-- Shared memory (RFC-1300). project_id NULL means global: visible to every
-- agent (RFC-1250 §11).
CREATE TABLE IF NOT EXISTS knowledge (
  knowledge_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  category TEXT NOT NULL,
  author TEXT NOT NULL REFERENCES agents(agent_id),
  content TEXT NOT NULL DEFAULT '{}',
  source TEXT,
  confidence REAL NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'proposed',
  tags TEXT NOT NULL DEFAULT '[]',
  refs TEXT NOT NULL DEFAULT '[]',
  project_id TEXT REFERENCES projects(project_id),
  task_id TEXT,
  supersedes TEXT,
  superseded_by TEXT,
  version INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_knowledge_scope ON knowledge(project_id, status, category);
CREATE INDEX IF NOT EXISTS idx_knowledge_task ON knowledge(task_id);

CREATE TABLE IF NOT EXISTS artifacts (
  artifact_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  type TEXT NOT NULL,
  uri TEXT NOT NULL,
  checksum TEXT NOT NULL,
  created_by TEXT NOT NULL REFERENCES agents(agent_id),
  task_id TEXT,
  project_id TEXT REFERENCES projects(project_id),
  size INTEGER NOT NULL DEFAULT 0,
  retention TEXT NOT NULL DEFAULT 'permanent',
  expires_at TEXT,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS verifications (
  verification_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  task_id TEXT NOT NULL REFERENCES tasks(task_id),
  verifier_id TEXT NOT NULL REFERENCES agents(agent_id),
  target_message_id TEXT NOT NULL REFERENCES messages(message_id),
  verdict TEXT NOT NULL,
  rationale TEXT,
  evidence TEXT NOT NULL DEFAULT '[]',
  ts TEXT NOT NULL
);

-- Operator escalations are durable so a Telegram reply can be routed back to
-- the exact agent that raised it, even after a server restart.
CREATE TABLE IF NOT EXISTS operator_requests (
  request_id TEXT PRIMARY KEY,
  agent_id TEXT NOT NULL REFERENCES agents(agent_id),
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  details TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending', -- pending|sent|answered|failed
  telegram_message_id INTEGER UNIQUE,
  reply TEXT,
  created_at TEXT NOT NULL,
  answered_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_operator_requests_status ON operator_requests(status, created_at);
CREATE TABLE IF NOT EXISTS telegram_updates (
 update_id INTEGER PRIMARY KEY,
 agent_id TEXT NOT NULL,
 message_id TEXT NOT NULL REFERENCES messages(message_id)
);

CREATE TABLE IF NOT EXISTS verification_jobs (
  task_id TEXT PRIMARY KEY REFERENCES tasks(task_id),
  message_id TEXT NOT NULL REFERENCES messages(message_id),
  attempts INTEGER NOT NULL DEFAULT 0,
  next_at TEXT NOT NULL,
  attempt_id TEXT NOT NULL DEFAULT '',
  lease_until TEXT NOT NULL DEFAULT '',
  done INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_verification_jobs_due ON verification_jobs(done, next_at);

CREATE TABLE IF NOT EXISTS verification_alerts (
  task_id TEXT PRIMARY KEY REFERENCES tasks(task_id),
  sent INTEGER NOT NULL DEFAULT 0,
  lease_until TEXT NOT NULL DEFAULT '',
  token TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS verification_retries (
  update_id INTEGER PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES tasks(task_id),
  created_at TEXT NOT NULL
);
