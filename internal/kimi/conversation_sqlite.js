const fs = require('node:fs');
const path = require('node:path');
const { randomUUID } = require('node:crypto');

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
  const db = new DatabaseSync(request.database, { readOnly: request.operation === 'list' });
  try {
    db.exec('PRAGMA busy_timeout = 3000; PRAGMA trusted_schema = OFF');
    const table = db.prepare("SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'conversations'").get();
    const columns = new Set(db.prepare('PRAGMA table_info(conversations)').all().map(row => row.name));
    const required = ['conversation_key', 'title', 'first_user_text', 'workspace_path', 'updated_at_ms', 'extra_json'];
    if (!table || !required.every(column => columns.has(column))) fail('conversation_database_schema_unsupported');
    return db;
  } catch (error) {
    db.close();
    throw error;
  }
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function listConversations(db, limit) {
  const rows = db.prepare(`SELECT conversation_key, title, substr(first_user_text, 1, 240) AS first_user_text,
    workspace_path, updated_at_ms, extra_json FROM conversations
    WHERE conversation_key IS NOT NULL AND conversation_key <> ''
    ORDER BY updated_at_ms DESC, conversation_key LIMIT ?`).all(limit);
  return { conversations: rows.map(row => {
    let preferences;
    try {
      preferences = JSON.parse(row.extra_json)?.runtimePreferences;
    } catch {
      preferences = null;
    }
    return {
      conversationKey: row.conversation_key,
      title: row.title,
      firstUserText: row.first_user_text,
      workspacePath: row.workspace_path,
      updatedAtMs: row.updated_at_ms,
      modelAlias: typeof preferences?.modelAlias === 'string' ? preferences.modelAlias : '',
      thinkingLevel: typeof preferences?.thinkingLevel === 'string' ? preferences.thinkingLevel : '',
    };
  }) };
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
  const temporary = path.join(path.dirname(filename), '.kwp-conversation-' + randomUUID());
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
  } finally {
    if (created) {
      try { fs.chmodSync(temporary, 0o600); } catch {}
      try { fs.unlinkSync(temporary); } catch {}
    }
  }
}

function rebindConversations(db, request) {
  const { agentDirectory, modelAlias, previousAlias } = request;
  const defaultLevel = request.thinkingLevel || '';
  const supportedEfforts = new Set(request.supportEfforts || []);
  const byok = modelAlias.startsWith('byok-');
  const written = [];
  let directoryCreated = false;
  db.exec('BEGIN IMMEDIATE');
  try {
    const read = db.prepare('SELECT extra_json FROM conversations WHERE conversation_key = ?');
    const update = db.prepare(`UPDATE conversations SET extra_json = CASE WHEN ? = '' THEN
      json_remove(json_set(COALESCE(NULLIF(extra_json, ''), '{}'), '$.runtimePreferences.modelAlias', ?), '$.runtimePreferences.thinkingLevel')
      ELSE json_set(COALESCE(NULLIF(extra_json, ''), '{}'), '$.runtimePreferences.modelAlias', ?, '$.runtimePreferences.thinkingLevel', ?)
      END WHERE conversation_key = ?`);
    const isObjectAt = db.prepare("SELECT json_type(?) AS root, json_type(?, '$.runtimePreferences') AS preferences");
    let conversationKeys = request.conversationKeys;
    let hasMore = false;
    if (previousAlias) {
      const matched = db.prepare(`SELECT conversation_key FROM conversations
        WHERE CASE WHEN json_valid(extra_json) THEN json_extract(extra_json, '$.runtimePreferences.modelAlias') END = ?
        ORDER BY updated_at_ms DESC, conversation_key LIMIT ?`).all(previousAlias, request.limit + 1);
      hasMore = matched.length > request.limit;
      conversationKeys = matched.slice(0, request.limit).map(row => row.conversation_key);
      if (conversationKeys.length === 0) {
        db.exec('ROLLBACK');
        return { conversationKeys, hadOfficial: false, hasMore };
      }
    }
    let hadOfficial = false;
    const snapshots = conversationKeys.map(key => {
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
      if (kind.root !== 'object' || (kind.preferences !== null && kind.preferences !== 'object')) {
        fail('conversation_preferences_invalid');
      }
      const currentAlias = parsed.runtimePreferences?.modelAlias;
      hadOfficial ||= typeof currentAlias !== 'string' || !currentAlias.startsWith('byok-');
      const previousLevel = parsed.runtimePreferences?.thinkingLevel;
      const level = previousAlias && supportedEfforts.has(previousLevel) ? previousLevel : defaultLevel;
      return { key, level };
    });
    const selection = snapshotFile(path.join(agentDirectory, 'conversation-kimi-work-models.json'));
    const bindings = snapshotFile(path.join(agentDirectory, 'byok-applied-model-bindings.json'));
    const selections = new Map(Object.entries(selection.document));
    const applied = new Map(Object.entries(bindings.document));
    for (const { key, level } of snapshots) {
      const previous = selections.get(key);
      selections.set(key, { ...(isObject(previous) ? previous : {}), modelKey: modelAlias, thinkingLevel: level });
      if (byok) applied.set(key, modelAlias);
      else applied.delete(key);
      if (update.run(level, modelAlias, modelAlias, level, key).changes !== 1) fail('conversation_not_found');
    }
    if (!fs.existsSync(agentDirectory)) {
      fs.mkdirSync(agentDirectory, { mode: 0o700 });
      directoryCreated = true;
    }
    for (const [snapshot, values] of [[selection, selections], [bindings, applied]]) {
      replaceFile(snapshot.filename, JSON.stringify(Object.fromEntries(values)), snapshot.mode);
      written.push(snapshot);
    }
    db.exec('COMMIT');
    return { conversationKeys, hadOfficial, hasMore };
  } catch (error) {
    let rollbackFailed = false;
    try {
      if (db.isTransaction) db.exec('ROLLBACK');
    } catch {
      rollbackFailed = true;
    }
    for (const snapshot of written.reverse()) {
      try {
        if (snapshot.data === null) fs.unlinkSync(snapshot.filename);
        else replaceFile(snapshot.filename, snapshot.data, snapshot.mode);
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
  const db = openDatabase(request);
  try {
    return request.operation === 'list' ? listConversations(db, request.limit) : rebindConversations(db, request);
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
