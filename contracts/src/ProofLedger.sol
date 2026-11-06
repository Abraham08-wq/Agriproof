// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title ProofLedger
/// @notice Anchors each cooperative's sealed record (ledger head hash) on-chain.
/// The backend keeps a hash-chained ledger (every entry includes the hash of the previous one).
/// Anchoring the latest head here makes any later edit to past entries detectable by anyone.
/// No money is held or moved by this contract.
contract ProofLedger {
    struct Anchor {
        bytes32 head; // hash of the latest ledger entry
        uint64 entries; // number of entries covered by `head`
        uint64 timestamp; // block time of anchoring
    }

    address public owner;
    mapping(address => bool) public relayers;
    mapping(bytes32 => Anchor) public latest; // groupId => latest anchor

    event RelayerSet(address indexed relayer, bool allowed);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);
    event Anchored(bytes32 indexed groupId, bytes32 head, uint64 entries, uint64 timestamp);

    error NotOwner();
    error NotRelayer();
    error ZeroAddress();
    error NonIncreasingEntries(uint64 previous, uint64 provided);

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    modifier onlyRelayer() {
        if (!relayers[msg.sender]) revert NotRelayer();
        _;
    }

    constructor(address initialRelayer) {
        if (initialRelayer == address(0)) revert ZeroAddress();
        owner = msg.sender;
        relayers[initialRelayer] = true;
        emit OwnershipTransferred(address(0), msg.sender);
        emit RelayerSet(initialRelayer, true);
    }

    function setRelayer(address relayer, bool allowed) external onlyOwner {
        if (relayer == address(0)) revert ZeroAddress();
        relayers[relayer] = allowed;
        emit RelayerSet(relayer, allowed);
    }

    function transferOwnership(address newOwner) external onlyOwner {
        if (newOwner == address(0)) revert ZeroAddress();
        emit OwnershipTransferred(owner, newOwner);
        owner = newOwner;
    }

    /// @notice Record the latest ledger head for a group. Entry count must always increase,
    /// so history can only be extended, never rewritten or rolled back.
    function anchor(bytes32 groupId, bytes32 head, uint64 entries) external onlyRelayer {
        uint64 previous = latest[groupId].entries;
        if (entries <= previous) revert NonIncreasingEntries(previous, entries);
        uint64 ts = uint64(block.timestamp);
        latest[groupId] = Anchor(head, entries, ts);
        emit Anchored(groupId, head, entries, ts);
    }
}
