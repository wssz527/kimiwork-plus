
function _0x1101() {
  const _0x1102 = ['fixture'];
  return _0x1102;
}
function _0x1103(_0x1104, _0x1105) {
  _0x1104 = _0x1104 - 0;
  return _0x1101()[_0x1104];
}
(function(_0x1106, _0x1107) {
  _0x1106();
})(_0x1101, 1);
function _0x1108(catalog) {
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
