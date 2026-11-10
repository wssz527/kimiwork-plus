
function _0x10a1() {
  const _0x10a2 = ['fixture'];
  return _0x10a2;
}
function _0x10a3(_0x10a4, _0x10a5) {
  _0x10a4 = _0x10a4 - 0;
  return _0x10a1()[_0x10a4];
}
(function(_0x10a6, _0x10a7) {
  _0x10a6();
})(_0x10a1, 1);
function _0x10a8(catalog) {
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
