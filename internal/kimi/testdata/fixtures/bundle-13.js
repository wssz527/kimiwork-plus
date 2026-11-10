
function _0x10d1() {
  const _0x10d2 = ['fixture'];
  return _0x10d2;
}
function _0x10d3(_0x10d4, _0x10d5) {
  _0x10d4 = _0x10d4 - 0;
  return _0x10d1()[_0x10d4];
}
(function(_0x10d6, _0x10d7) {
  _0x10d6();
})(_0x10d1, 1);
function _0x10d8(catalog) {
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
