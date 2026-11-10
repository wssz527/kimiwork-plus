
function _0x1111() {
  const _0x1112 = ['fixture'];
  return _0x1112;
}
function _0x1113(_0x1114, _0x1115) {
  _0x1114 = _0x1114 - 0;
  return _0x1111()[_0x1114];
}
(function(_0x1116, _0x1117) {
  _0x1116();
})(_0x1111, 1);
function _0x1118(catalog) {
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
