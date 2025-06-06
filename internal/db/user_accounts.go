package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/glasskube/distr/internal/apierrors"
	internalctx "github.com/glasskube/distr/internal/context"
	"github.com/glasskube/distr/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	userAccountOutputExpr = "u.id, u.created_at, u.email, u.email_verified_at, u.password_hash, " +
		"u.password_salt, u.name, u.image_id"
	userAccountWithRoleOutputExpr = userAccountOutputExpr +
		", j.user_role, j.created_at "
	userAccountWithRoleOutputExprWithAlias = userAccountWithRoleOutputExpr + " as joined_org_at "
)

func CreateUserAccountWithOrganization(
	ctx context.Context,
	userAccount *types.UserAccount,
) (*types.Organization, error) {
	org := types.Organization{
		Name: userAccount.Email,
	}
	if err := CreateUserAccount(ctx, userAccount); err != nil {
		return nil, err
	} else if err := CreateOrganization(ctx, &org); err != nil {
		return nil, err
	} else if err := CreateUserAccountOrganizationAssignment(
		ctx,
		userAccount.ID,
		org.ID,
		types.UserRoleVendor,
	); err != nil {
		return nil, err
	} else {
		return &org, nil
	}
}

func CreateUserAccount(ctx context.Context, userAccount *types.UserAccount) error {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		"INSERT INTO UserAccount AS u (email, password_hash, password_salt, name, email_verified_at) "+
			"VALUES (@email, @password_hash, @password_salt, @name, @email_verified_at) "+
			"RETURNING "+userAccountOutputExpr,
		pgx.NamedArgs{
			"email":             userAccount.Email,
			"password_hash":     userAccount.PasswordHash,
			"password_salt":     userAccount.PasswordSalt,
			"name":              userAccount.Name,
			"email_verified_at": userAccount.EmailVerifiedAt,
		},
	)
	if err != nil {
		return fmt.Errorf("could not query users: %w", err)
	} else if created, err := pgx.CollectExactlyOneRow[types.UserAccount](rows, pgx.RowToStructByName); err != nil {
		if pgerr := (*pgconn.PgError)(nil); errors.As(err, &pgerr) && pgerr.Code == pgerrcode.UniqueViolation {
			return fmt.Errorf("user account with email %v can not be created: %w", userAccount.Email, apierrors.ErrAlreadyExists)
		}
		return fmt.Errorf("could not create user: %w", err)
	} else {
		*userAccount = created
		return nil
	}
}

func UpdateUserAccount(ctx context.Context, userAccount *types.UserAccount) error {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`UPDATE UserAccount AS u
		SET email = @email,
			name = @name,
			password_hash = @password_hash,
			password_salt = @password_salt,
			email_verified_at = @email_verified_at
		WHERE id = @id
		RETURNING `+userAccountOutputExpr,
		pgx.NamedArgs{
			"id":                userAccount.ID,
			"email":             userAccount.Email,
			"password_hash":     userAccount.PasswordHash,
			"password_salt":     userAccount.PasswordSalt,
			"name":              userAccount.Name,
			"email_verified_at": userAccount.EmailVerifiedAt,
		},
	)
	if err != nil {
		return fmt.Errorf("could not query users: %w", err)
	} else if created, err := pgx.CollectExactlyOneRow[types.UserAccount](rows, pgx.RowToStructByName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apierrors.ErrNotFound
		} else if pgerr := (*pgconn.PgError)(nil); errors.As(err, &pgerr) && pgerr.Code == pgerrcode.UniqueViolation {
			return fmt.Errorf("can not update user with email %v: %w", userAccount.Email, apierrors.ErrAlreadyExists)
		}
		return fmt.Errorf("could not update user: %w", err)
	} else {
		*userAccount = created
		return nil
	}
}

func UpdateUserAccountEmailVerified(ctx context.Context, userAccount *types.UserAccount) error {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		`UPDATE UserAccount AS u
		SET email_verified_at = CURRENT_TIMESTAMP
		WHERE id = @id
		RETURNING `+userAccountOutputExpr,
		pgx.NamedArgs{
			"id": userAccount.ID,
		},
	)
	if err != nil {
		return fmt.Errorf("could not query users: %w", err)
	} else if created, err := pgx.CollectExactlyOneRow[types.UserAccount](rows, pgx.RowToStructByName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return apierrors.ErrNotFound
		} else if pgerr := (*pgconn.PgError)(nil); errors.As(err, &pgerr) && pgerr.Code == pgerrcode.UniqueViolation {
			return fmt.Errorf("can not update user with email %v: %w", userAccount.Email, apierrors.ErrAlreadyExists)
		}
		return fmt.Errorf("could not update user: %w", err)
	} else {
		*userAccount = created
		return nil
	}
}

func DeleteUserAccountWithID(ctx context.Context, id uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	cmd, err := db.Exec(ctx, `DELETE FROM UserAccount WHERE id = @id`, pgx.NamedArgs{"id": id})
	if err != nil {
		if pgerr := (*pgconn.PgError)(nil); errors.As(err, &pgerr) && pgerr.Code == pgerrcode.ForeignKeyViolation {
			err = fmt.Errorf("%w: %w", apierrors.ErrConflict, err)
		}
	} else if cmd.RowsAffected() == 0 {
		err = apierrors.ErrNotFound
	}

	if err != nil {
		return fmt.Errorf("could not delete UserAccount: %w", err)
	}

	return nil
}

func UserManagesDeploymentTargetInOrganization(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx, `
		SELECT count(dt.id) > 0
		FROM DeploymentTarget dt
		WHERE dt.organization_id = @orgId AND dt.created_by_user_account_id = @userId`,
		pgx.NamedArgs{"orgId": orgID, "userId": userID},
	)
	if err != nil {
		return false, err
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[struct{ Exists bool }])
	if err != nil {
		return false, err
	}
	return result.Exists, nil
}

func UserOwnsApplicationLicensesInOrganization(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx, `
		SELECT count(al.id) > 0
		FROM ApplicationLicense al
		WHERE al.organization_id = @orgId AND al.owner_useraccount_id = @userId`,
		pgx.NamedArgs{"orgId": orgID, "userId": userID},
	)
	if err != nil {
		return false, err
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[struct{ Exists bool }])
	if err != nil {
		return false, err
	}
	return result.Exists, nil
}

func UserOwnsArtifactLicensesInOrganization(ctx context.Context, userID, orgID uuid.UUID) (bool, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx, `
		SELECT count(al.id) > 0
		FROM ArtifactLicense al
		WHERE al.organization_id = @orgId AND al.owner_useraccount_id = @userId`,
		pgx.NamedArgs{"orgId": orgID, "userId": userID},
	)
	if err != nil {
		return false, err
	}
	result, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[struct{ Exists bool }])
	if err != nil {
		return false, err
	}
	return result.Exists, nil
}

func DeleteUserAccountFromOrganization(ctx context.Context, userID, orgID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	cmd, err := db.Exec(ctx, `
		DELETE FROM Organization_UserAccount
		WHERE user_account_id = @userId AND organization_id = @orgId`,
		pgx.NamedArgs{"userId": userID, "orgId": orgID})
	if err == nil && cmd.RowsAffected() == 0 {
		err = apierrors.ErrNotFound
	}
	return err
}

func CreateUserAccountOrganizationAssignment(ctx context.Context, userID, orgID uuid.UUID, role types.UserRole) error {
	db := internalctx.GetDb(ctx)
	_, err := db.Exec(ctx,
		"INSERT INTO Organization_UserAccount (organization_id, user_account_id, user_role) VALUES (@orgId, @userId, @role)",
		pgx.NamedArgs{"userId": userID, "orgId": orgID, "role": role},
	)
	if pgerr := (*pgconn.PgError)(nil); errors.As(err, &pgerr) && pgerr.Code == pgerrcode.UniqueViolation {
		return apierrors.ErrAlreadyExists
	}
	return err
}

func GetUserAccountsByOrgID(ctx context.Context, orgID uuid.UUID, role *types.UserRole) (
	[]types.UserAccountWithUserRole,
	error,
) {
	db := internalctx.GetDb(ctx)
	checkRole := role != nil
	rows, err := db.Query(ctx,
		"SELECT "+userAccountWithRoleOutputExprWithAlias+`
		FROM UserAccount u
		INNER JOIN Organization_UserAccount j ON u.id = j.user_account_id
		WHERE j.organization_id = @orgId AND (NOT @checkRole OR j.user_role = @role)
		ORDER BY u.name, u.email`,
		pgx.NamedArgs{"orgId": orgID, "checkRole": checkRole, "role": role},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query users: %w", err)
	} else if result, err := pgx.CollectRows[types.UserAccountWithUserRole](rows, pgx.RowToStructByName); err != nil {
		return nil, fmt.Errorf("could not map users: %w", err)
	} else {
		return result, nil
	}
}

func GetUserAccountByID(ctx context.Context, id uuid.UUID) (*types.UserAccount, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		"SELECT "+userAccountOutputExpr+" FROM UserAccount u WHERE u.id = @id",
		pgx.NamedArgs{"id": id},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query users: %w", err)
	} else if userAccount, err := pgx.CollectExactlyOneRow[types.UserAccount](rows, pgx.RowToStructByName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.ErrNotFound
		} else {
			return nil, fmt.Errorf("could not map user: %w", err)
		}
	} else {
		return &userAccount, nil
	}
}

func GetUserAccountByEmail(ctx context.Context, email string) (*types.UserAccount, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		"SELECT "+userAccountOutputExpr+" FROM UserAccount u WHERE u.email = @email",
		pgx.NamedArgs{"email": email},
	)
	if err != nil {
		return nil, fmt.Errorf("could not query users: %w", err)
	} else if userAccount, err := pgx.CollectExactlyOneRow[types.UserAccount](rows, pgx.RowToStructByName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.ErrNotFound
		} else {
			return nil, fmt.Errorf("could not map user: %w", err)
		}
	} else {
		return &userAccount, nil
	}
}

func GetUserAccountWithRole(ctx context.Context, userID, orgID uuid.UUID) (*types.UserAccountWithUserRole, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		"SELECT "+userAccountWithRoleOutputExprWithAlias+`
			FROM UserAccount u
			INNER JOIN Organization_UserAccount j ON u.id = j.user_account_id
			WHERE u.id = @id AND j.organization_id = @orgId`,
		pgx.NamedArgs{"id": userID, "orgId": orgID},
	)
	if err != nil {
		return nil, err
	}
	userAccount, err := pgx.CollectExactlyOneRow[types.UserAccountWithUserRole](rows, pgx.RowToStructByName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apierrors.ErrNotFound
		} else {
			return nil, fmt.Errorf("could not map user: %w", err)
		}
	} else {
		return &userAccount, nil
	}
}

func GetUserAccountAndOrg(ctx context.Context, userID, orgID uuid.UUID, expectedRole *types.UserRole) (
	*types.UserAccountWithUserRole,
	*types.Organization,
	error,
) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		"SELECT ("+userAccountWithRoleOutputExpr+`),
					(`+organizationOutputExpr+`)
			FROM UserAccount u
			INNER JOIN Organization_UserAccount j ON u.id = j.user_account_id
			INNER JOIN Organization o ON o.id = j.organization_id
			WHERE u.id = @id AND j.organization_id = @orgId AND (NOT @checkRole OR j.user_role = @role)`,
		pgx.NamedArgs{
			"id":        userID,
			"orgId":     orgID,
			"role":      expectedRole,
			"checkRole": expectedRole != nil,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	res, err := pgx.CollectExactlyOneRow[struct {
		User types.UserAccountWithUserRole
		Org  types.Organization
	}](rows, pgx.RowToStructByPos)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, apierrors.ErrNotFound
		} else {
			return nil, nil, fmt.Errorf("could not map user or org: %w", err)
		}
	} else {
		return &res.User, &res.Org, nil
	}
}

func GetUserAccountAndOrgForDeploymentTarget(
	ctx context.Context,
	id uuid.UUID,
) (*types.UserAccountWithUserRole, *types.Organization, error) {
	db := internalctx.GetDb(ctx)
	rows, err := db.Query(ctx,
		"SELECT ("+userAccountWithRoleOutputExpr+`),
					(`+organizationOutputExpr+`)
			FROM DeploymentTarget dt
			JOIN Organization o ON o.id = dt.organization_id
			JOIN UserAccount u ON u.id = dt.created_by_user_account_id
			JOIN Organization_UserAccount j ON u.id = j.user_account_id
				AND o.id = j.organization_id
			WHERE dt.id = @id`,
		pgx.NamedArgs{"id": id},
	)
	if err != nil {
		return nil, nil, err
	}
	res, err := pgx.CollectExactlyOneRow[struct {
		User types.UserAccountWithUserRole
		Org  types.Organization
	}](rows, pgx.RowToStructByPos)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, apierrors.ErrNotFound
		} else {
			return nil, nil, fmt.Errorf("could not map user or org: %w", err)
		}
	} else {
		return &res.User, &res.Org, nil
	}
}

func UpdateUserAccountLastLoggedIn(ctx context.Context, userID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	cmd, err := db.Exec(
		ctx,
		`UPDATE UserAccount SET last_logged_in_at = now() WHERE id = @id`,
		pgx.NamedArgs{"id": userID},
	)
	if err == nil && cmd.RowsAffected() == 0 {
		err = apierrors.ErrNotFound
	}
	if err != nil {
		err = fmt.Errorf("could not update last_logged_in_at on UserAccount: %w", err)
	}
	return err
}

func UpdateUserAccountImage(ctx context.Context, userAccount *types.UserAccountWithUserRole, imageID uuid.UUID) error {
	db := internalctx.GetDb(ctx)
	row := db.QueryRow(ctx,
		`UPDATE UserAccount SET image_id = @imageId WHERE id = @id RETURNING image_id`,
		pgx.NamedArgs{"imageId": imageID, "id": userAccount.ID},
	)
	if err := row.Scan(&userAccount.ImageID); err != nil {
		return fmt.Errorf("could not save image id to user account: %w", err)
	}
	return nil
}
