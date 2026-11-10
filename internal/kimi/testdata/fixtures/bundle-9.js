
function _0x1091() {
  const _0x1092 = ['fixture'];
  return _0x1092;
}
function _0x1093(_0x1094, _0x1095) {
  _0x1094 = _0x1094 - 0;
  return _0x1091()[_0x1094];
}
(function(_0x1096, _0x1097) {
  _0x1096();
})(_0x1091, 1);
function _0x1098(catalog) {
  const models = [];
  const skippedModels = [];
  for (const group of Object.values(catalog)) {
    for (const [id, value] of Object.entries(group.models)) {
      if (id.includes('reject')) { skippedModels.push(id); continue; }
      const name = id.toLowerCase().split('/').pop();
      models.push({ model: id, family: name.startsWith('kimi-') ? 'kimi' : name.startsWith('gpt-') ? 'gpt' : 'generic', maxContextSize: value.limit.context });
    }
  }
  return { models, skippedModels };
}
// byok-availability.json byok-providers.json models/api.json
