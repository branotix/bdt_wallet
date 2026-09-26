const hre = require("hardhat");

async function main() {
  const INITIAL_SUPPLY = process.env.INITIAL_SUPPLY || "1000000"; // whole tokens

  const BDTToken = await hre.ethers.getContractFactory("BDTToken");
  const token = await BDTToken.deploy(INITIAL_SUPPLY);
  await token.waitForDeployment();

  const address = await token.getAddress();
  console.log("BDTToken deployed to:", address);
  console.log("Set this as BDT_TOKEN_ADDRESS in your relayer's .env");
  console.log("Next: npx hardhat verify --network <network>", address, INITIAL_SUPPLY);
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
