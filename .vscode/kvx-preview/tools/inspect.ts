// Quick terminal inspector: prints a parsed summary for a .kvx file.
//   npm run inspect -- path/to/spec.kvx
import { readFileSync } from "node:fs";
import { buildModel } from "../src/kvx/model";

const path = process.argv[2];
if (!path) {
  console.error("usage: npm run inspect -- <file.kvx>");
  process.exit(1);
}

const model = buildModel(readFileSync(path, "utf8"), path);

if (model.kind === "spec") {
  console.log(`spec: ${model.feature} (${model.status})`);
  console.log(`  requirements=${model.requirements.length} tasks=${model.tasks.length} waves=[${model.waves.join(",")}]`);
  const c = model.coverage;
  console.log(`  coverage: ${c.coveredCriteria}/${c.totalCriteria} criteria covered`);
  if (c.gaps.length) console.log(`  gaps (${c.gaps.length}): ${c.gaps.join(", ")}`);
  if (c.dangling.length) console.log(`  dangling: ${c.dangling.map((d) => `${d.task}->${d.ref}`).join(", ")}`);
} else if (model.kind === "workflow") {
  console.log(`workflow: ${model.name}`);
  console.log(`  loop=${model.loop.length} principles=${model.principles.length} hardRules=${model.hardRules.length} adapters=${model.adapters.length}`);
} else {
  console.log(`generic: ${model.sections.length} sections`);
}
