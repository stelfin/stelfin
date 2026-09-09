#![cfg(test)]

//! Tests for the connector registry.
//!
//! Same discipline as the treasury's: the checks that decide who may change a
//! grant are asserted with one signature mocked, not with all of them, because
//! a suite written under `mock_all_auths` would pass against a contract with no
//! authorisation at all.

use soroban_sdk::testutils::{Address as _, Ledger, MockAuth, MockAuthInvoke};
use soroban_sdk::{symbol_short, vec, Address, BytesN, Env, IntoVal, Symbol};

use crate::types::{Error, Grant, Standing};
use crate::{ConnectorRegistry, ConnectorRegistryClient};

const DAY: u64 = 86_400;
const NOW: u64 = 1_700_000_000;

struct Fixture<'a> {
    env: Env,
    client: ConnectorRegistryClient<'a>,
    contract: Address,
    admin: Address,
    sheets: Symbol,
    surface: BytesN<32>,
    endpoint: BytesN<32>,
}

fn setup() -> Fixture<'static> {
    let env = Env::default();
    env.ledger().set_timestamp(NOW);

    let admin = Address::generate(&env);
    let contract = env.register(ConnectorRegistry, ());
    let client = ConnectorRegistryClient::new(&env, &contract);

    env.mock_all_auths();
    client.init(&admin);

    Fixture {
        surface: BytesN::from_array(&env, &[1u8; 32]),
        endpoint: BytesN::from_array(&env, &[2u8; 32]),
        sheets: symbol_short!("sheets"),
        env,
        client,
        contract,
        admin,
    }
}

fn grant_for(f: &Fixture) -> Grant {
    Grant {
        capabilities: vec![&f.env, symbol_short!("read"), symbol_short!("propose")],
        surface_digest: f.surface.clone(),
        endpoint_hash: f.endpoint.clone(),
        per_call_limit: 1_000,
        daily_limit: 5_000,
        not_before: NOW,
        not_after: NOW + 30 * DAY,
    }
}

#[test]
fn a_grant_is_recorded_and_readable() {
    let f = setup();
    f.env.mock_all_auths();

    f.client.grant(&f.sheets, &grant_for(&f));

    assert_eq!(f.client.grant_of(&f.sheets), grant_for(&f));
    assert_eq!(f.client.standing(&f.sheets), Standing::Active);
    assert_eq!(f.client.connectors(), vec![&f.env, f.sheets.clone()]);
}

/// Only the DAO grants capabilities.
///
/// The whole design: an operator who can grant capabilities to itself has not
/// been constrained by anything.
#[test]
fn only_the_admin_grants() {
    let f = setup();
    let intruder = Address::generate(&f.env);

    f.env.mock_auths(&[MockAuth {
        address: &intruder,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "grant",
            args: (f.sheets.clone(), grant_for(&f)).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    assert!(
        f.client.try_grant(&f.sheets, &grant_for(&f)).is_err(),
        "a stranger granted itself a capability"
    );
    assert_eq!(f.client.standing(&f.sheets), Standing::None);
}

#[test]
fn only_the_admin_revokes() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    let intruder = Address::generate(&f.env);
    f.env.mock_auths(&[MockAuth {
        address: &intruder,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "revoke",
            args: (f.sheets.clone(), symbol_short!("nope")).into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    assert!(
        f.client
            .try_revoke(&f.sheets, &symbol_short!("nope"))
            .is_err(),
        "a stranger revoked a DAO's grant"
    );
    assert_eq!(f.client.standing(&f.sheets), Standing::Active);
}

/// A grant that says something it does not mean is refused.
#[test]
fn an_incoherent_grant_is_refused() {
    let f = setup();
    f.env.mock_all_auths();
    let base = grant_for(&f);

    let cases = [
        // A window that never opens.
        Grant {
            not_after: NOW,
            ..base.clone()
        },
        Grant {
            not_after: NOW - 1,
            ..base.clone()
        },
        Grant {
            per_call_limit: -1,
            ..base.clone()
        },
        Grant {
            daily_limit: -1,
            ..base.clone()
        },
        // A per-call limit above the daily one can never apply, and reads to a
        // person as the real limit.
        Grant {
            per_call_limit: 6_000,
            ..base.clone()
        },
    ];

    for grant in cases {
        let err = f
            .client
            .try_grant(&f.sheets, &grant)
            .err()
            .unwrap()
            .unwrap();
        assert_eq!(err, Error::InvalidGrant, "accepted {grant:?}");
    }
}

/// Revocation is a fact that is kept, not a deletion.
///
/// Deleting the grant would make "revoked" and "never granted" the same
/// observation, and an auditor asking why a connector stopped working deserves
/// an answer.
#[test]
fn a_revocation_says_who_and_why() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    f.env.ledger().set_timestamp(NOW + 100);
    f.client.revoke(&f.sheets, &symbol_short!("leaked"));

    assert_eq!(f.client.standing(&f.sheets), Standing::Revoked);
    let record = f.client.revocation(&f.sheets);
    assert_eq!(record.reason, symbol_short!("leaked"));
    assert_eq!(record.by, f.admin);
    assert_eq!(record.at, NOW + 100);
    // And the grant is still readable, so what was withdrawn is on the record
    // too.
    assert_eq!(f.client.grant_of(&f.sheets), grant_for(&f));

    let err = f
        .client
        .try_revoke(&f.sheets, &symbol_short!("again"))
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::AlreadyRevoked);
}

/// Granting again after a revocation is the DAO deciding again.
#[test]
fn a_regrant_clears_the_revocation_and_the_days_usage() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));
    // Four calls at the per-call limit, so most of the day is used up.
    for _ in 0..4 {
        f.client
            .authorise(&f.sheets, &f.surface, &f.endpoint, &1_000);
    }
    assert_eq!(f.client.remaining(&f.sheets), 1_000);

    f.client.revoke(&f.sheets, &symbol_short!("rotate"));
    f.client.grant(&f.sheets, &grant_for(&f));

    assert_eq!(f.client.standing(&f.sheets), Standing::Active);
    // Usage does not carry: the limits may have changed, and counting old
    // spending against a new limit is arithmetic about two different things.
    assert_eq!(f.client.remaining(&f.sheets), 5_000);
}

#[test]
fn a_grant_has_a_window() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(
        &f.sheets,
        &Grant {
            not_before: NOW + DAY,
            not_after: NOW + 2 * DAY,
            ..grant_for(&f)
        },
    );

    assert_eq!(f.client.standing(&f.sheets), Standing::NotYet);
    let err = f
        .client
        .try_authorise(&f.sheets, &f.surface, &f.endpoint, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::NotActive);

    f.env.ledger().set_timestamp(NOW + DAY);
    assert_eq!(f.client.standing(&f.sheets), Standing::Active);
    f.client.authorise(&f.sheets, &f.surface, &f.endpoint, &1);

    f.env.ledger().set_timestamp(NOW + 2 * DAY);
    assert_eq!(f.client.standing(&f.sheets), Standing::Expired);
    let err = f
        .client
        .try_authorise(&f.sheets, &f.surface, &f.endpoint, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::NotActive);
}

/// The pin that a capability list cannot provide.
///
/// A server granted "read a spreadsheet" that later answers the same call with
/// a tool transferring funds passes every capability check. The digest is what
/// turns that into a refusal.
#[test]
fn a_changed_tool_surface_is_refused() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    let changed = BytesN::from_array(&f.env, &[9u8; 32]);
    let err = f
        .client
        .try_authorise(&f.sheets, &changed, &f.endpoint, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::SurfaceChanged);

    // And it consumed nothing on the way to being refused.
    assert_eq!(f.client.remaining(&f.sheets), 5_000);
}

#[test]
fn a_different_endpoint_is_refused() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    let elsewhere = BytesN::from_array(&f.env, &[8u8; 32]);
    let err = f
        .client
        .try_authorise(&f.sheets, &f.surface, &elsewhere, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::WrongEndpoint);
    assert_eq!(f.client.remaining(&f.sheets), 5_000);
}

#[test]
fn limits_apply_per_call_and_per_day() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    let err = f
        .client
        .try_authorise(&f.sheets, &f.surface, &f.endpoint, &1_001)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::OverLimit);

    for _ in 0..5 {
        f.client
            .authorise(&f.sheets, &f.surface, &f.endpoint, &1_000);
    }
    assert_eq!(f.client.remaining(&f.sheets), 0);

    let err = f
        .client
        .try_authorise(&f.sheets, &f.surface, &f.endpoint, &1)
        .err()
        .unwrap()
        .unwrap();
    assert_eq!(err, Error::OverLimit);

    // The day rolls forward from the first call, not from a clock boundary.
    f.env.ledger().set_timestamp(NOW + DAY + 1);
    assert_eq!(f.client.remaining(&f.sheets), 5_000);
}

/// A refused call must not consume the allowance it was refused for.
#[test]
fn a_refused_call_costs_nothing() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    for amount in [-1i128, 1_001, i128::MAX] {
        assert!(f
            .client
            .try_authorise(&f.sheets, &f.surface, &f.endpoint, &amount)
            .is_err());
    }
    assert_eq!(f.client.remaining(&f.sheets), 5_000);
}

/// Authorising needs the admin's signature.
///
/// Without it, a stranger could burn a connector's daily allowance by calling
/// this in a loop — a denial of service that costs the attacker one fee.
#[test]
fn authorising_needs_the_admins_signature() {
    let f = setup();
    f.env.mock_all_auths();
    f.client.grant(&f.sheets, &grant_for(&f));

    let stranger = Address::generate(&f.env);
    f.env.mock_auths(&[MockAuth {
        address: &stranger,
        invoke: &MockAuthInvoke {
            contract: &f.contract,
            fn_name: "authorise",
            args: (
                f.sheets.clone(),
                f.surface.clone(),
                f.endpoint.clone(),
                1_000i128,
            )
                .into_val(&f.env),
            sub_invokes: &[],
        },
    }]);
    assert!(f
        .client
        .try_authorise(&f.sheets, &f.surface, &f.endpoint, &1_000)
        .is_err());
    assert_eq!(f.client.remaining(&f.sheets), 5_000);
}

#[test]
fn an_unknown_connector_is_not_a_grant() {
    let f = setup();
    assert_eq!(f.client.standing(&symbol_short!("nobody")), Standing::None);
    assert_eq!(
        f.client
            .try_grant_of(&symbol_short!("nobody"))
            .err()
            .unwrap()
            .unwrap(),
        Error::UnknownGrant
    );
}

#[test]
fn init_happens_once() {
    let f = setup();
    f.env.mock_all_auths();
    let other = Address::generate(&f.env);
    assert_eq!(
        f.client.try_init(&other).err().unwrap().unwrap(),
        Error::AlreadyInitialised
    );
    assert_eq!(f.client.admin(), f.admin);
}
