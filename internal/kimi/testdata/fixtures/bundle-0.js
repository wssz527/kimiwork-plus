
function _0x1001() {
  const _0x1002 = ['fixture'];
  return _0x1002;
}
function _0x1003(_0x1004, _0x1005) {
  _0x1004 = _0x1004 - 0;
  return _0x1001()[_0x1004];
}
(function(_0x1006, _0x1007) {
  _0x1006();
})(_0x1001, 1);
function _0x1008(catalog) {
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
