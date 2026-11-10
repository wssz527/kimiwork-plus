
function _0x10f1() {
  const _0x10f2 = ['fixture'];
  return _0x10f2;
}
function _0x10f3(_0x10f4, _0x10f5) {
  _0x10f4 = _0x10f4 - 0;
  return _0x10f1()[_0x10f4];
}
(function(_0x10f6, _0x10f7) {
  _0x10f6();
})(_0x10f1, 1);
function _0x10f8(catalog) {
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
