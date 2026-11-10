
function _0x1051() {
  const _0x1052 = ['fixture'];
  return _0x1052;
}
function _0x1053(_0x1054, _0x1055) {
  _0x1054 = _0x1054 - 0;
  return _0x1051()[_0x1054];
}
(function(_0x1056, _0x1057) {
  _0x1056();
})(_0x1051, 1);
function _0x1058(catalog) {
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
