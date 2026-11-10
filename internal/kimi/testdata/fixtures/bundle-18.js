
function _0x1121() {
  const _0x1122 = ['fixture'];
  return _0x1122;
}
function _0x1123(_0x1124, _0x1125) {
  _0x1124 = _0x1124 - 0;
  return _0x1121()[_0x1124];
}
(function(_0x1126, _0x1127) {
  _0x1126();
})(_0x1121, 1);
function _0x1128(catalog) {
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
