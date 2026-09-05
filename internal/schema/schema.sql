CREATE TABLE IF NOT EXISTS agents (
  agent_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  capabilities TEXT NOT NULL DEFAULT '[]',
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

CREATE TABLE IF NOT EXISTS tasks (
  task_id TEXT PRIMARY KEY,
  schema_version TEXT NOT NULL DEFAULT '1.0',
  objective TEXT NOT NULL,
  context TEXT NOT NULL DEFAULT '{}',
  constraints TEXT NOT NULL DEFAULT '[]',
  required_capabilities TEXT NOT NULL DEFAULT '[]',
  success_criteria TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'OPEN',
  owner TEXT REFERENCES agents(agent_id),
  team_id TEXT,
  project_id TEXT,
  created_by TEXT NOT NULL REFERENCES agents(agent_id),
  idempotency_key TEXT,
  created_at TEXT NOT NULL,
  deadline TEXT
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
