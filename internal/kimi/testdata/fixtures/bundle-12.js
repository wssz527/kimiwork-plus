
function _0x10c1() {
  const _0x10c2 = ['fixture'];
  return _0x10c2;
}
function _0x10c3(_0x10c4, _0x10c5) {
  _0x10c4 = _0x10c4 - 0;
  return _0x10c1()[_0x10c4];
}
(function(_0x10c6, _0x10c7) {
  _0x10c6();
})(_0x10c1, 1);
function _0x10c8(catalog) {
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
