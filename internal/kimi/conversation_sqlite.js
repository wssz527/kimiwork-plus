const fs = require('node:fs');
const path = require('node:path');
const { randomUUID } = require('node:crypto');

const JOURNAL_NAME = '.kwp-conversation-rebind.journal.json';

function fail(code) {
  throw new Error(code);
}

function openDatabase(request) {
  let DatabaseSync;
  try {
    ({ DatabaseSync } = require('node:sqlite'));
  } catch {
    fail('conversation_node_unavailable');
  }
  if (!fs.statSync(request.database).isFile()) fail('conversation_database_unavailable');
  const readOnly = request.operation === 'list' || request.operation === 'recover';
  const db = new DatabaseSync(request.database, { readOnly });
  try {
    db.exec('PRAGMA busy_timeout = 3000; PRAGMA trusted_schema = OFF');
    const meta = db.prepare("SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'schema_metadata'").get();
    if (meta) {
      const version = db.prepare("SELECT value FROM schema_metadata WHERE key = 'schema_version'").get();
      if (!version || version.value !== '1') fail('conversation_database_schema_unsupported');
    }
    const table = db.prepare("SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'conversations'").get();
    const columns = new Set(db.prepare('PRAGMA table_info(conversations)').all().map(row => row.name));
    const required = ['agent_id', 'conversation_key', 'title', 'first_user_text', 'workspace_path', 'updated_at_ms', 'extra_json'];
    if (!table || !required.every(column => columns.has(column))) fail('conversation_database_schema_unsupported');
    return { db, visibleWhere: visibleWhereClause(columns.has('kernel_records_path')) };
  } catch (error) {
    db.close();
    throw error;
  }
}

function visibleWhereClause(hasKernelRecordsPath) {
  const onboarding = hasKernelRecordsPath
    ? "(json_extract(extra_json, '$.onboardingImport') IS NULL OR (json_type(extra_json, '$.onboardingImport.completedAt') = 'text' AND kernel_records_path IS NOT NULL))"
    : "(json_extract(extra_json, '$.onboardingImport') IS NULL OR json_type(extra_json, '$.onboardingImport.completedAt') = 'text')";
  return [
    "agent_id = 'main'",
    "conversation_key IS NOT NULL AND conversation_key <> ''",
    "CASE WHEN json_valid(extra_json) THEN json_type(extra_json, '$.temporaryConversation') IS NULL ELSE 1 END",
    `CASE WHEN json_valid(extra_json) THEN ${onboarding} ELSE 1 END`,
    "CASE WHEN json_valid(extra_json) THEN (json_type(extra_json, '$.forkCreation') IS NULL OR json_extract(extra_json, '$.forkCreation.state') = 'ready') ELSE 1 END",
  ].join(' AND ');
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function parsePreferences(extra) {
  try {
    const preferences = JSON.parse(extra || '{}')?.runtimePreferences;
    return {
      modelAlias: typeof preferences?.modelAlias === 'string' ? preferences.modelAlias : '',
      thinkingLevel: typeof preferences?.thinkingLevel === 'string' ? preferences.thinkingLevel : '',
    };
  } catch {
    return { modelAlias: '', thinkingLevel: '' };
  }
}

function migrationMatcher(request) {
  const exact = new Map();
  const gateway = new Map();
  const foreign = new Set((request.foreignProviders || []).map(id => id.toLowerCase()));
  for (const migration of request.migrations || []) {
    for (const alias of migration.aliases || []) {
      const previous = exact.get(alias);
      if (previous === undefined) exact.set(alias, migration);
      else if (previous?.targetModelAlias !== migration.targetModelAlias) exact.set(alias, null);
    }
    if (migration.gatewayModelId) gateway.set(migration.gatewayModelId, migration);
  }
  return {
    find(alias) {
      if (exact.has(alias)) return exact.get(alias);
      const match = /^(byok-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})\/(.+)$/i.exec(alias);
      if (match && !foreign.has(match[1].toLowerCase())) return gateway.get(match[2]);
    },
    candidates(db, visibleWhere, order = '') {
      return db.prepare(`SELECT conversation_key,
        CASE WHEN json_valid(extra_json) THEN json_extract(extra_json, '$.runtimePreferences.modelAlias') END AS alias
        FROM conversations WHERE ${visibleWhere} AND typeof(alias) = 'text'
        AND (alias IN (SELECT value FROM json_each(?)) OR
          (substr(alias, 1, 5) = 'byok-' AND substr(alias, 43) IN (SELECT value FROM json_each(?))))
        ${order}`).iterate(JSON.stringify([...exact.keys()]), JSON.stringify([...gateway.keys()]));
    },
  };
}

function listConversations(db, request, visibleWhere) {
  const migration = migrationMatcher(request);
  const migrationCounts = Object.create(null);
  if (request.migrations?.length) {
    for (const row of migration.candidates(db, visibleWhere)) {
      const target = migration.find(row.alias)?.targetModelAlias;
      if (target) migrationCounts[target] = (migrationCounts[target] || 0) + 1;
    }
  }
  const rows = db.prepare(`SELECT conversation_key, title, substr(first_user_text, 1, 240) AS first_user_text,
    workspace_path, updated_at_ms, extra_json FROM conversations
    WHERE ${visibleWhere}
    ORDER BY updated_at_ms DESC, conversation_key LIMIT ?`).all(request.limit);
  return { migrationCounts, conversations: rows.map(row => {
    const preferences = parsePreferences(row.extra_json);
    return {
      conversationKey: row.conversation_key,
      title: row.title,
      firstUserText: row.first_user_text,
      workspacePath: row.workspace_path,
      updatedAtMs: row.updated_at_ms,
      modelAlias: preferences.modelAlias,
      migrationTargetAlias: migration.find(preferences.modelAlias)?.targetModelAlias,
      thinkingLevel: preferences.thinkingLevel,
    };
  }) };
}

function fsyncDirectory(directory) {
  if (process.platform === 'win32') return;
  let fd;
  try {
    fd = fs.openSync(directory, 'r');
    fs.fsyncSync(fd);
  } catch {
    // Best-effort for filesystems that do not support directory fsync.
  } finally {
    if (fd !== undefined) {
      try { fs.closeSync(fd); } catch {}
    }
  }
}

function snapshotFile(filename) {
  let info;
  try {
    info = fs.lstatSync(filename);
  } catch (error) {
    if (error.code === 'ENOENT') return { filename, data: null, mode: 0o600, document: {} };
    throw error;
  }
  if (!info.isFile() || info.size > 32 * 1024 * 1024) fail('conversation_bindings_invalid');
  if (process.platform === 'win32' && (info.mode & 0o200) === 0) fail('conversation_bindings_read_only');
  const data = fs.readFileSync(filename);
  let document;
  try {
    document = JSON.parse(data.toString('utf8'), (_key, value, source) =>
      typeof value === 'number' ? JSON.rawJSON(source.source) : value);
  } catch {
    fail('conversation_bindings_invalid');
  }
  if (!isObject(document)) fail('conversation_bindings_invalid');
  return { filename, data, mode: info.mode & 0o777, document };
}

function replaceFile(filename, data, mode) {
  const directory = path.dirname(filename);
  const temporary = path.join(directory, '.kwp-conversation-' + randomUUID());
  const windows = process.platform === 'win32';
  let created = false;
  try {
    const fd = fs.openSync(temporary, 'wx', windows ? 0o600 : mode | 0o200);
    created = true;
    try {
      fs.writeFileSync(fd, data);
      if (!windows) fs.fchmodSync(fd, mode);
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(temporary, filename);
    created = false;
    fsyncDirectory(directory);
  } finally {
    if (created) {
      try { fs.chmodSync(temporary, 0o600); } catch {}
      try { fs.unlinkSync(temporary); } catch {}
    }
  }
}

function removeJournal(journalPath) {
  try {
    fs.unlinkSync(journalPath);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    return;
  }
  fsyncDirectory(path.dirname(journalPath));
}

function applyJsonBindings(agentDirectory, entries, preloaded) {
  const [selection, bindings] = preloaded || [
    snapshotFile(path.join(agentDirectory, 'conversation-kimi-work-models.json')),
    snapshotFile(path.join(agentDirectory, 'byok-applied-model-bindings.json')),
  ];
  const selections = new Map(Object.entries(selection.document));
  const applied = new Map(Object.entries(bindings.document));
  for (const entry of entries) {
    const previous = selections.get(entry.key);
    const next = { ...(isObject(previous) ? previous : {}), modelKey: entry.modelAlias };
    if (entry.byok) next.thinkingLevel = entry.thinkingLevel;
    else {
      delete next.thinkingLevel;
      delete next.pendingRuntime;
    }
    selections.set(entry.key, next);
    if (entry.byok) applied.set(entry.key, entry.modelAlias);
    else applied.delete(entry.key);
  }
  replaceFile(selection.filename, JSON.stringify(Object.fromEntries(selections)), selection.mode);
  replaceFile(bindings.filename, JSON.stringify(Object.fromEntries(applied)), bindings.mode);
}

function recoverPendingJournal(db, agentDirectory, visibleWhere) {
  if (!agentDirectory) return { recovered: 'none' };
  const journalPath = path.join(agentDirectory, JOURNAL_NAME);
  let raw;
  try {
    if (!fs.lstatSync(journalPath).isFile()) fail('conversation_rollback_failed');
    raw = fs.readFileSync(journalPath, 'utf8');
  } catch (error) {
    if (error.code === 'ENOENT') return { recovered: 'none' };
    fail('conversation_rollback_failed');
  }
  let journal;
  try {
    journal = JSON.parse(raw);
  } catch {
    fail('conversation_rollback_failed');
  }
  if (!isObject(journal) || !Array.isArray(journal.entries)) fail('conversation_rollback_failed');
  if (journal.entries.length === 0) {
    removeJournal(journalPath);
    return { recovered: 'rolled_back' };
  }
  const read = db.prepare(`SELECT extra_json FROM conversations WHERE ${visibleWhere} AND conversation_key = ?`);
  let committedChange = false;
  let anyStillOld = false;
  const matchingEntries = [];
  for (const entry of journal.entries) {
    if (!isObject(entry) || typeof entry.key !== 'string' || typeof entry.modelAlias !== 'string') {
      fail('conversation_rollback_failed');
    }
    const level = typeof entry.thinkingLevel === 'string' ? entry.thinkingLevel : '';
    const oldAlias = typeof entry.oldAlias === 'string' ? entry.oldAlias : '';
    const oldLevel = typeof entry.oldLevel === 'string' ? entry.oldLevel : '';
    const row = read.get(entry.key);
    if (!row) continue;
    const current = parsePreferences(row.extra_json);
    const changed = oldAlias !== entry.modelAlias || oldLevel !== level;
    if (current.modelAlias === entry.modelAlias && current.thinkingLevel === level) {
      committedChange ||= changed;
      matchingEntries.push({ key: entry.key, modelAlias: entry.modelAlias, thinkingLevel: level, byok: Boolean(entry.byok) });
    } else if (current.modelAlias === oldAlias && current.thinkingLevel === oldLevel) {
      anyStillOld = true;
    }
  }
  if (!matchingEntries.length || (!committedChange && anyStillOld)) {
    removeJournal(journalPath);
    return { recovered: 'rolled_back' };
  }
  applyJsonBindings(agentDirectory, matchingEntries);
  removeJournal(journalPath);
  return {
    recovered: 'committed',
    conversationKeys: matchingEntries.map(entry => entry.key),
    hadOfficial: Boolean(journal.hadOfficial),
    hasMore: Boolean(journal.hasMore),
  };
}

function rebindConversations(db, request, visibleWhere) {
  const { agentDirectory } = request;
  const journalPath = path.join(agentDirectory, JOURNAL_NAME);
  recoverPendingJournal(db, agentDirectory, visibleWhere);

  let directoryCreated = false;
  let journalWritten = false;
  let committed = false;
  db.exec('BEGIN IMMEDIATE');
  try {
    const read = db.prepare(`SELECT extra_json FROM conversations WHERE ${visibleWhere} AND conversation_key = ?`);
    const normalizedExtra = `CASE WHEN json_type(COALESCE(NULLIF(extra_json, ''), '{}'), '$.runtimePreferences') = 'null'
      THEN json_set(COALESCE(NULLIF(extra_json, ''), '{}'), '$.runtimePreferences', json('{}'))
      ELSE COALESCE(NULLIF(extra_json, ''), '{}') END`;
    const update = db.prepare(`UPDATE conversations SET extra_json = CASE WHEN ? = '' THEN
      json_remove(json_set(${normalizedExtra}, '$.runtimePreferences.modelAlias', ?), '$.runtimePreferences.thinkingLevel')
      ELSE json_set(${normalizedExtra}, '$.runtimePreferences.modelAlias', ?, '$.runtimePreferences.thinkingLevel', ?)
      END WHERE ${visibleWhere} AND conversation_key = ?`);
    const isObjectAt = db.prepare("SELECT json_type(?) AS root, json_type(?, '$.runtimePreferences') AS preferences");
    const migrations = new Map();
    let conversationKeys = request.conversationKeys || [];
    let hasMore = false;
    if (request.migrations?.length) {
      const matcher = migrationMatcher(request);
      conversationKeys = [];
      for (const row of matcher.candidates(db, visibleWhere, 'ORDER BY updated_at_ms DESC, conversation_key')) {
        const migration = matcher.find(row.alias);
        if (!migration) continue;
        if (conversationKeys.length >= request.limit) {
          hasMore = true;
          break;
        }
        migrations.set(row.conversation_key, migration);
        conversationKeys.push(row.conversation_key);
      }
    }
    let hadOfficial = false;
    const entries = [];
    for (const key of conversationKeys) {
      const rows = read.all(key);
      if (rows.length !== 1) fail('conversation_not_found');
      const extra = rows[0].extra_json;
      let parsed;
      try {
        parsed = JSON.parse(extra || '{}');
      } catch {
        fail('conversation_preferences_invalid');
      }
      const text = extra || '{}';
      const kind = isObjectAt.get(text, text);
      if (kind.root !== 'object' || (kind.preferences !== null && kind.preferences !== 'null' && kind.preferences !== 'object')) {
        fail('conversation_preferences_invalid');
      }
      const currentAlias = typeof parsed.runtimePreferences?.modelAlias === 'string' ? parsed.runtimePreferences.modelAlias : '';
      const previousLevel = typeof parsed.runtimePreferences?.thinkingLevel === 'string' ? parsed.runtimePreferences.thinkingLevel : '';
      const migration = migrations.get(key);
      const modelAlias = migration?.targetModelAlias || request.modelAlias;
      const byok = modelAlias.startsWith('byok-');
      const level = byok ? (migration?.supportEfforts?.includes(previousLevel) ? previousLevel :
        (migration?.thinkingLevel ?? request.thinkingLevel ?? '')) : '';
      if (currentAlias === modelAlias && previousLevel === level) continue;
      hadOfficial ||= !currentAlias || !currentAlias.startsWith('byok-');
      entries.push({ key, oldAlias: currentAlias, oldLevel: previousLevel, modelAlias, thinkingLevel: level, byok });
    }
    conversationKeys = entries.map(entry => entry.key);
    if (!entries.length) {
      db.exec('ROLLBACK');
      return { conversationKeys, hadOfficial, hasMore };
    }
    const preloaded = [
      snapshotFile(path.join(agentDirectory, 'conversation-kimi-work-models.json')),
      snapshotFile(path.join(agentDirectory, 'byok-applied-model-bindings.json')),
    ];
    for (const entry of entries) {
      if (update.run(entry.thinkingLevel, entry.modelAlias, entry.modelAlias, entry.thinkingLevel, entry.key).changes !== 1) {
        fail('conversation_not_found');
      }
    }
    if (!fs.existsSync(agentDirectory)) {
      fs.mkdirSync(agentDirectory, { mode: 0o700 });
      fsyncDirectory(path.dirname(agentDirectory));
      directoryCreated = true;
    }
    replaceFile(journalPath, JSON.stringify({ version: 1, entries, hadOfficial, hasMore }), 0o600);
    journalWritten = true;
    db.exec('COMMIT');
    committed = true;
    applyJsonBindings(agentDirectory, entries, preloaded);
    removeJournal(journalPath);
    return { conversationKeys, hadOfficial, hasMore };
  } catch (error) {
    if (committed) fail('conversation_rollback_failed');
    let rollbackFailed = false;
    try {
      if (db.isTransaction) db.exec('ROLLBACK');
    } catch {
      rollbackFailed = true;
    }
    if (journalWritten) {
      try {
        removeJournal(journalPath);
      } catch {
        rollbackFailed = true;
      }
    }
    if (directoryCreated) {
      try {
        fs.rmdirSync(agentDirectory);
      } catch {
        rollbackFailed = true;
      }
    }
    if (rollbackFailed) fail('conversation_rollback_failed');
    throw error;
  }
}

function main(request) {
  const { db, visibleWhere } = openDatabase(request);
  try {
    if (request.operation === 'recover') return recoverPendingJournal(db, request.agentDirectory, visibleWhere);
    if (request.operation === 'list') return listConversations(db, request, visibleWhere);
    return rebindConversations(db, request, visibleWhere);
  } finally {
    db.close();
  }
}

try {
  const request = JSON.parse(fs.readFileSync(0, 'utf8'));
  process.stdout.write(JSON.stringify(main(request)));
} catch (error) {
  const code = /^conversation_[a-z_]+$/.test(error.message) ? error.message :
    error.code === 'ERR_SQLITE_ERROR' && (error.errcode & 255) === 5 ? 'conversation_database_busy' :
    error.code === 'ENOENT' && error.path?.endsWith('conversations.sqlite') ? 'conversation_database_missing' :
    'conversation_database_unavailable';
  process.stdout.write(JSON.stringify({ error: code }));
}
